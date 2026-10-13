// Package azure implements blob.Store on Azure Blob Storage (and Azurite).
//
// Authentication, in order of precedence:
//
//  1. cfg.ConnectionString (Azurite, shared key or account-SAS connection
//     strings; "UseDevelopmentStorage=true" selects local Azurite),
//  2. cfg.Account + cfg.AccountKey (shared key),
//  3. azidentity.DefaultAzureCredential (managed identity, workload
//     identity, Azure CLI, environment).
//
// Presigned URLs are service SAS URLs signed with the shared key when one is
// available, otherwise user-delegation SAS URLs (the identity needs
// "Storage Blob Delegator", included in "Storage Blob Data Contributor").
// Account-SAS connection strings cannot presign.
//
// On accounts with a hierarchical namespace (ADLS Gen2), directory
// placeholder blobs (metadata hdi_isfolder=true) are hidden from listings.
package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	azb "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/internal/blobutil"
	"github.com/thasami/backyard/internal/config"
)

const (
	// singleUploadMax is the largest body sent with one Put Blob request;
	// larger or unknown-size bodies are staged as blocks.
	singleUploadMax = 8 << 20
	blockSize       = 4 << 20
	uploadWorkers   = 4
	deleteWorkers   = 16
	deletePage      = 1000
	// clockSkew backdates SAS start times.
	clockSkew = 5 * time.Minute
	// delegationKeyLifetime is how long requested user delegation keys are
	// valid (they are cached and reused for many SAS URLs).
	delegationKeyLifetime = 24 * time.Hour
	maxDelegationKey      = 7*24*time.Hour - time.Minute
	folderMetadataKey     = "hdi_isfolder"

	// azuriteAccount and azuriteKey are Azurite's well-known development
	// credentials (public, documented by Microsoft).
	azuriteAccount  = "devstoreaccount1"
	azuriteKey      = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	azuriteEndpoint = "http://127.0.0.1:10000/devstoreaccount1"
)

// Option configures a Store.
type Option func(*options)

type options struct {
	cred       azcore.TokenCredential
	clientOpts []func(*service.ClientOptions)
	logger     *slog.Logger
}

// WithCredential sets the token credential used when neither a connection
// string nor an account key is configured (default DefaultAzureCredential).
func WithCredential(cred azcore.TokenCredential) Option {
	return func(o *options) { o.cred = cred }
}

// WithClientOptions adjusts the Azure SDK client options.
func WithClientOptions(fn func(*service.ClientOptions)) Option {
	return func(o *options) { o.clientOpts = append(o.clientOpts, fn) }
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// Store is a blob.Store backed by an Azure Blob Storage container (optionally
// under a key prefix). It is safe for concurrent use.
type Store struct {
	svc       *service.Client
	cont      *container.Client
	container string
	prefix    string
	account   string
	endpoint  string // non-default service endpoint, for Describe
	sharedKey *service.SharedKeyCredential
	tokenAuth bool
	protocol  sas.Protocol
	log       *slog.Logger

	udcMu     sync.Mutex
	udc       *service.UserDelegationCredential
	udcExpiry time.Time
}

var _ blob.Store = (*Store)(nil)

// New creates a Store from configuration. It does not contact Azure; use
// Ping to verify access.
func New(ctx context.Context, cfg config.AzureConfig, opts ...Option) (*Store, error) {
	if strings.TrimSpace(cfg.Container) == "" {
		return nil, errors.New("blob/azure: container is required")
	}
	o := options{logger: slog.Default()}
	for _, fn := range opts {
		fn(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	clientOpts := &service.ClientOptions{}
	for _, fn := range o.clientOpts {
		fn(clientOpts)
	}

	s := &Store{container: cfg.Container, prefix: blobutil.NormPrefix(cfg.Prefix)}
	var (
		serviceURL string
		err        error
	)
	switch {
	case cfg.ConnectionString != "":
		cs, perr := parseConnectionString(cfg.ConnectionString)
		if perr != nil {
			return nil, fmt.Errorf("blob/azure: %w", perr)
		}
		serviceURL = firstNonEmpty(cfg.Endpoint, cs.serviceURL)
		if err := checkEndpoint(serviceURL); err != nil {
			return nil, err
		}
		s.account = firstNonEmpty(cs.accountName, cfg.Account)
		switch {
		case cs.accountKey != "":
			if s.account == "" {
				return nil, errors.New("blob/azure: connection string has AccountKey but no AccountName")
			}
			if s.sharedKey, err = service.NewSharedKeyCredential(s.account, cs.accountKey); err != nil {
				return nil, fmt.Errorf("blob/azure: shared key: %w", err)
			}
			s.svc, err = service.NewClientWithSharedKeyCredential(ensureSlash(serviceURL), s.sharedKey, clientOpts)
		case cs.sas != "":
			s.svc, err = service.NewClientWithNoCredential(ensureSlash(serviceURL)+"?"+strings.TrimPrefix(cs.sas, "?"), clientOpts)
		default:
			return nil, errors.New("blob/azure: connection string needs AccountKey or SharedAccessSignature")
		}
	case cfg.AccountKey != "":
		if cfg.Account == "" {
			return nil, errors.New("blob/azure: account is required with account_key")
		}
		s.account = cfg.Account
		serviceURL = firstNonEmpty(cfg.Endpoint, defaultServiceURL(cfg.Account))
		if err := checkEndpoint(serviceURL); err != nil {
			return nil, err
		}
		if s.sharedKey, err = service.NewSharedKeyCredential(cfg.Account, cfg.AccountKey); err != nil {
			return nil, fmt.Errorf("blob/azure: shared key: %w", err)
		}
		s.svc, err = service.NewClientWithSharedKeyCredential(ensureSlash(serviceURL), s.sharedKey, clientOpts)
	default:
		if cfg.Account == "" {
			return nil, errors.New("blob/azure: account (or connection_string) is required")
		}
		s.account = cfg.Account
		serviceURL = firstNonEmpty(cfg.Endpoint, defaultServiceURL(cfg.Account))
		if err := checkEndpoint(serviceURL); err != nil {
			return nil, err
		}
		cred := o.cred
		if cred == nil {
			if cred, err = azidentity.NewDefaultAzureCredential(nil); err != nil {
				return nil, fmt.Errorf("blob/azure: default credential: %w", err)
			}
		}
		s.tokenAuth = true
		s.svc, err = service.NewClient(ensureSlash(serviceURL), cred, clientOpts)
	}
	if err != nil {
		return nil, fmt.Errorf("blob/azure: create client: %w", err)
	}
	u, err := url.Parse(serviceURL)
	if err != nil {
		return nil, fmt.Errorf("blob/azure: invalid endpoint %q", blobutil.RedactURL(serviceURL))
	}
	s.protocol = sas.ProtocolHTTPS
	if u.Scheme == "http" {
		s.protocol = sas.ProtocolHTTPSandHTTP
	}
	if s.account == "" || ensureSlash(serviceURL) != defaultServiceURL(s.account)+"/" {
		s.endpoint = blobutil.PublicEndpoint(serviceURL)
	}
	s.cont = s.svc.NewContainerClient(cfg.Container)
	s.log = o.logger.With("component", "blob", "backend", "azure", "container", cfg.Container)
	return s, nil
}

// checkEndpoint validates a blob service URL (without echoing credentials
// it may carry).
func checkEndpoint(serviceURL string) error {
	u, err := url.Parse(serviceURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("blob/azure: invalid endpoint %q (want https://<account>.blob.core.windows.net)", blobutil.RedactURL(serviceURL))
	}
	return nil
}

func defaultServiceURL(account string) string {
	return "https://" + account + ".blob.core.windows.net"
}

func ensureSlash(u string) string {
	if strings.HasSuffix(u, "/") {
		return u
	}
	return u + "/"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type connString struct {
	serviceURL, accountName, accountKey, sas string
}

// parseConnectionString parses an Azure Storage connection string.
func parseConnectionString(cs string) (connString, error) {
	kv := map[string]string{}
	for part := range strings.SplitSeq(cs, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return connString{}, errors.New("malformed connection string")
		}
		kv[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if strings.EqualFold(kv["usedevelopmentstorage"], "true") {
		return connString{serviceURL: azuriteEndpoint, accountName: azuriteAccount, accountKey: azuriteKey}, nil
	}
	out := connString{accountName: kv["accountname"], accountKey: kv["accountkey"], sas: kv["sharedaccesssignature"]}
	switch {
	case kv["blobendpoint"] != "":
		out.serviceURL = kv["blobendpoint"]
	case out.accountName != "":
		proto := firstNonEmpty(kv["defaultendpointsprotocol"], "https")
		suffix := firstNonEmpty(kv["endpointsuffix"], "core.windows.net")
		out.serviceURL = fmt.Sprintf("%s://%s.blob.%s", proto, out.accountName, suffix)
	default:
		return connString{}, errors.New("connection string needs AccountName or BlobEndpoint")
	}
	if out.accountKey == "" && out.sas == "" {
		return connString{}, errors.New("connection string needs AccountKey or SharedAccessSignature")
	}
	return out, nil
}

// Client exposes the underlying container client (for tests and tooling).
func (s *Store) Client() *container.Client { return s.cont }

func (s *Store) name(key string) string { return s.prefix + key }

// checkKey validates key, including its length once prefixed.
func (s *Store) checkKey(key string) error { return blobutil.CheckFullKey(s.prefix, key) }

func isNotFound(err error) bool {
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return true
	}
	if bloberror.HasCode(err, bloberror.ContainerNotFound) {
		return false
	}
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound && re.ErrorCode == ""
}

func statusCode(err error) int {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode
	}
	return 0
}

func (s *Store) wrap(ctx context.Context, op, key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %q", blob.ErrNotFound, key)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("blob/azure: %s %q: %w", op, key, err)
}

func objectInfo(key string, size *int64, etag *azcore.ETag, ct *string, mod *time.Time) *blob.ObjectInfo {
	var (
		n int64
		m time.Time
		e string
	)
	if size != nil {
		n = *size
	}
	if mod != nil {
		m = mod.UTC()
	}
	if etag != nil {
		e = string(*etag)
	}
	info := &blob.ObjectInfo{Key: key, Size: n, ETag: blobutil.ETagOr(e, n, m), ModTime: m}
	if ct != nil {
		info.ContentType = *ct
	}
	return info
}

// Get opens an object for reading. Transient failures while reading are
// retried from the current offset.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, *blob.ObjectInfo, error) {
	return s.get(ctx, key, 0, -1)
}

// GetRange reads length bytes from offset (length < 0 = to the end). The
// returned ObjectInfo describes the whole object; ranges past the end yield
// an empty body.
func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := blobutil.CheckRange(offset); err != nil {
		return nil, nil, err
	}
	return s.get(ctx, key, offset, length)
}

func (s *Store) get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, nil, err
	}
	if length == 0 {
		info, err := s.Stat(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		return blobutil.EmptyBody(), info, nil
	}
	opts := &azb.DownloadStreamOptions{}
	if offset > 0 || length > 0 {
		opts.Range = azb.HTTPRange{Offset: offset, Count: max(length, 0)}
	}
	resp, err := s.cont.NewBlobClient(s.name(key)).DownloadStream(ctx, opts)
	if err != nil {
		if statusCode(err) == http.StatusRequestedRangeNotSatisfiable || bloberror.HasCode(err, bloberror.InvalidRange) {
			info, serr := s.Stat(ctx, key)
			if serr != nil {
				return nil, nil, serr
			}
			if offset >= info.Size {
				return blobutil.EmptyBody(), info, nil
			}
		}
		return nil, nil, s.wrap(ctx, "get", key, err)
	}
	info := objectInfo(key, resp.ContentLength, resp.ETag, resp.ContentType, resp.LastModified)
	if resp.ContentRange != nil {
		if total, ok := blobutil.ParseContentRangeTotal(*resp.ContentRange); ok {
			info.Size = total
		}
	}
	return resp.NewRetryReader(ctx, &azb.RetryReaderOptions{MaxRetries: 3}), info, nil
}

// Stat returns object metadata or blob.ErrNotFound.
func (s *Store) Stat(ctx context.Context, key string) (*blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	p, err := s.cont.NewBlobClient(s.name(key)).GetProperties(ctx, nil)
	if err != nil {
		return nil, s.wrap(ctx, "stat", key, err)
	}
	return objectInfo(key, p.ContentLength, p.ETag, p.ContentType, p.LastModified), nil
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Put writes a block blob. Bodies up to 8 MiB are sent in one request;
// larger or unknown-size bodies are staged in 4 MiB blocks. When contentType
// is empty it is derived from the key extension or content.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("blob/azure: put: nil reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := blobutil.NewBody(blobutil.ContextReader(ctx, blobutil.ExactSize(r, size)))
	ct, err := body.ContentType(key, contentType)
	if err != nil {
		return nil, fmt.Errorf("blob/azure: put %q: %w", key, err)
	}
	headers := &azb.HTTPHeaders{BlobContentType: to.Ptr(ct)}
	bb := s.cont.NewBlockBlobClient(s.name(key))
	chunk, last, err := body.NextChunk(singleUploadMax, size)
	if err != nil {
		return nil, fmt.Errorf("blob/azure: put %q: %w", key, err)
	}
	var (
		etag  *azcore.ETag
		mod   *time.Time
		total int64
	)
	if last {
		resp, err := bb.Upload(ctx, streaming.NopCloser(bytes.NewReader(chunk)), &blockblob.UploadOptions{HTTPHeaders: headers})
		if err != nil {
			return nil, s.wrap(ctx, "put", key, err)
		}
		etag, mod, total = resp.ETag, resp.LastModified, int64(len(chunk))
	} else {
		cr := &countingReader{r: io.MultiReader(bytes.NewReader(chunk), body.Reader())}
		resp, err := bb.UploadStream(ctx, cr, &blockblob.UploadStreamOptions{
			BlockSize:   blockSize,
			Concurrency: uploadWorkers,
			HTTPHeaders: headers,
		})
		if err != nil {
			return nil, s.wrap(ctx, "put", key, err)
		}
		etag, mod, total = resp.ETag, resp.LastModified, cr.n.Load()
	}
	info := objectInfo(key, &total, etag, &ct, mod)
	if mod == nil {
		info.ModTime = time.Now().UTC()
	}
	return info, nil
}

// Delete removes an object (and its snapshots); deleting a missing object is
// not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	return s.deleteName(ctx, s.name(key))
}

func (s *Store) deleteName(ctx context.Context, name string) error {
	_, err := s.cont.NewBlobClient(name).Delete(ctx, &azb.DeleteOptions{
		DeleteSnapshots: to.Ptr(azb.DeleteSnapshotsOptionTypeInclude),
	})
	if err != nil && !isNotFound(err) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("blob/azure: delete %q: %w", name, err)
	}
	return nil
}

func isFolder(md map[string]*string) bool {
	for k, v := range md {
		if strings.EqualFold(k, folderMetadataKey) && v != nil && strings.EqualFold(*v, "true") {
			return true
		}
	}
	return false
}

// DeletePrefix removes every object whose key starts with prefix (deletes
// run concurrently; HNS directory placeholders are removed last, deepest
// first). An empty prefix is rejected.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if err := blobutil.CheckDeletePrefix(prefix); err != nil {
		return err
	}
	pager := s.cont.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:  to.Ptr(s.name(prefix)),
		Include: container.ListBlobsInclude{Metadata: true},
	})
	var (
		names   = make([]string, 0, deletePage)
		folders []string
	)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("blob/azure: delete prefix %q: list: %w", prefix, err)
		}
		for _, it := range page.Segment.BlobItems {
			if it == nil || it.Name == nil {
				continue
			}
			if isFolder(it.Metadata) {
				folders = append(folders, *it.Name)
				continue
			}
			names = append(names, *it.Name)
		}
		if len(names) >= deletePage {
			if err := blobutil.ForEach(ctx, names, deleteWorkers, s.deleteName); err != nil {
				return fmt.Errorf("blob/azure: delete prefix %q: %w", prefix, err)
			}
			names = names[:0]
		}
	}
	if err := blobutil.ForEach(ctx, names, deleteWorkers, s.deleteName); err != nil {
		return fmt.Errorf("blob/azure: delete prefix %q: %w", prefix, err)
	}
	sort.Slice(folders, func(i, j int) bool { return strings.Count(folders[i], "/") > strings.Count(folders[j], "/") })
	for _, f := range folders {
		if err := s.deleteName(ctx, f); err != nil {
			return fmt.Errorf("blob/azure: delete prefix %q: %w", prefix, err)
		}
	}
	return nil
}

// List returns all objects under prefix, sorted by key. With delimiter "/",
// keys below the next "/" are folded into Prefixes.
func (s *Store) List(ctx context.Context, prefix, delimiter string) (*blob.ListResult, error) {
	if err := blobutil.CheckPrefix(prefix); err != nil {
		return nil, err
	}
	if err := blobutil.CheckDelimiter(delimiter); err != nil {
		return nil, err
	}
	res := &blob.ListResult{}
	addItem := func(it *container.BlobItem) {
		if it == nil || it.Name == nil || isFolder(it.Metadata) {
			return
		}
		key, ok := strings.CutPrefix(*it.Name, s.prefix)
		if !ok || !blobutil.ListableKey(key) {
			return
		}
		var info *blob.ObjectInfo
		if p := it.Properties; p != nil {
			info = objectInfo(key, p.ContentLength, p.ETag, p.ContentType, p.LastModified)
		} else {
			info = objectInfo(key, nil, nil, nil, nil)
		}
		res.Objects = append(res.Objects, *info)
	}
	include := container.ListBlobsInclude{Metadata: true}
	full := to.Ptr(s.name(prefix))
	if delimiter == "" {
		pager := s.cont.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: full, Include: include})
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, s.listErr(ctx, prefix, err)
			}
			for _, it := range page.Segment.BlobItems {
				addItem(it)
			}
		}
	} else {
		pager := s.cont.NewListBlobsHierarchyPager(delimiter, &container.ListBlobsHierarchyOptions{Prefix: full, Include: include})
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, s.listErr(ctx, prefix, err)
			}
			for _, it := range page.Segment.BlobItems {
				addItem(it)
			}
			for _, bp := range page.Segment.BlobPrefixes {
				if bp == nil || bp.Name == nil {
					continue
				}
				if p, ok := strings.CutPrefix(*bp.Name, s.prefix); ok && p != "" {
					res.Prefixes = append(res.Prefixes, p)
				}
			}
		}
	}
	blobutil.SortListResult(res)
	return res, nil
}

func (s *Store) listErr(ctx context.Context, prefix string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("blob/azure: list %q: %w", prefix, err)
}

// userDelegation returns a cached user delegation credential valid until at
// least `until` (capped at the 7-day maximum), fetching a new key if needed.
// It also returns the key's expiry, which bounds SAS expiry.
func (s *Store) userDelegation(ctx context.Context, until time.Time) (*service.UserDelegationCredential, time.Time, error) {
	s.udcMu.Lock()
	defer s.udcMu.Unlock()
	now := time.Now().UTC()
	limit := now.Add(maxDelegationKey)
	if until.After(limit) {
		until = limit
	}
	if s.udc != nil && !s.udcExpiry.Before(until) {
		return s.udc, s.udcExpiry, nil
	}
	exp := now.Add(delegationKeyLifetime)
	if exp.Before(until) {
		exp = until
	}
	if exp.After(limit) {
		exp = limit
	}
	udc, err := s.svc.GetUserDelegationCredential(ctx, service.KeyInfo{
		Start:  to.Ptr(now.Add(-clockSkew).Format(sas.TimeFormat)),
		Expiry: to.Ptr(exp.Format(sas.TimeFormat)),
	}, nil)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("get user delegation key: %w", err)
	}
	// Formatting drops sub-second precision; keep the conservative value.
	exp = exp.Truncate(time.Second)
	s.udc, s.udcExpiry = udc, exp
	return udc, exp, nil
}

func (s *Store) signSAS(ctx context.Context, key string, perms sas.BlobPermissions, ttl time.Duration) (string, time.Time, error) {
	if s.sharedKey == nil && !s.tokenAuth {
		return "", time.Time{}, fmt.Errorf("%w: Azure SAS URLs need an account key or token credentials", blob.ErrNotSupported)
	}
	ttl = blobutil.ClampTTL(ttl)
	now := time.Now().UTC()
	expiry := now.Add(ttl).Truncate(time.Second)
	vals := sas.BlobSignatureValues{
		Protocol:      s.protocol,
		StartTime:     now.Add(-clockSkew),
		ExpiryTime:    expiry,
		Permissions:   perms.String(),
		ContainerName: s.container,
		BlobName:      s.name(key),
	}
	var (
		qp  sas.QueryParameters
		err error
	)
	if s.sharedKey != nil {
		qp, err = vals.SignWithSharedKey(s.sharedKey)
	} else {
		udc, keyExp, uerr := s.userDelegation(ctx, expiry)
		if uerr != nil {
			return "", time.Time{}, uerr
		}
		if expiry.After(keyExp) {
			expiry = keyExp
			vals.ExpiryTime = expiry
		}
		qp, err = vals.SignWithUserDelegation(udc)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	u := s.cont.NewBlobClient(s.name(key)).URL()
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	return u + "?" + qp.Encode(), expiry, nil
}

// PresignPut returns a SAS URL accepting a PUT of the object's bytes. The
// returned Headers (x-ms-blob-type: BlockBlob and Content-Type) must be sent
// verbatim.
func (s *Store) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	ct := blobutil.PresignContentType(key, contentType)
	u, exp, err := s.signSAS(ctx, key, sas.BlobPermissions{Create: true, Write: true}, ttl)
	if err != nil {
		if errors.Is(err, blob.ErrNotSupported) {
			return nil, err
		}
		return nil, fmt.Errorf("blob/azure: presign put %q: %w", key, err)
	}
	return &blob.PresignedRequest{
		Method: http.MethodPut,
		URL:    u,
		Headers: map[string]string{
			"x-ms-blob-type": "BlockBlob",
			"Content-Type":   ct,
		},
		ExpiresAt: exp,
	}, nil
}

// PresignGet returns a read-only SAS URL.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	u, exp, err := s.signSAS(ctx, key, sas.BlobPermissions{Read: true}, ttl)
	if err != nil {
		if errors.Is(err, blob.ErrNotSupported) {
			return nil, err
		}
		return nil, fmt.Errorf("blob/azure: presign get %q: %w", key, err)
	}
	return &blob.PresignedRequest{Method: http.MethodGet, URL: u, ExpiresAt: exp}, nil
}

// Ping checks container access (Get Container Properties), falling back to
// listing one blob when the identity may only access blob data.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.cont.GetProperties(ctx, nil)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if statusCode(err) == http.StatusForbidden {
		pager := s.cont.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
			Prefix: to.Ptr(s.prefix), MaxResults: to.Ptr(int32(1)),
		})
		_, lerr := pager.NextPage(ctx)
		if lerr == nil {
			return nil
		}
		err = lerr
	}
	return fmt.Errorf("blob/azure: ping container %q: %w", s.container, err)
}

// Describe returns the public storage description.
func (s *Store) Describe() blob.Description {
	return blob.Description{
		Type:      "azure",
		Container: s.container,
		Account:   s.account,
		Prefix:    s.prefix,
		Endpoint:  s.endpoint,
	}
}

// Close releases resources; the Azure clients need no explicit shutdown.
func (s *Store) Close() error { return nil }
