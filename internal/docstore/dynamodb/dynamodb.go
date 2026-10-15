// Package dynamodb is the docstore.Store for Amazon DynamoDB (and DynamoDB
// Local / LocalStack for development).
//
// # Table layout
//
// One table with a string partition key "pk" and a string sort key "sk":
//
//   - documents: pk = "<site>#<collection>", sk = <id>, plus attributes
//     data (S, the JSON body — preserving JSON types exactly), created_at and
//     updated_at (N, unix nanoseconds), created_by / updated_by (S) and
//     version (N).
//   - collection counters: pk = "<site>#__collections", sk = <collection>,
//     n (N), maintained with atomic ADD on every create and delete. Collections
//     lists counters with n > 0 and Count sums them. Counters are updated
//     right after the document write (not transactionally, to avoid
//     transaction conflicts on hot collections), so a crash between the two
//     calls can leave a counter off by one; Count is only used for the soft
//     per-site quota.
//
// "#" and "__collections" cannot clash with valid site or collection names.
//
// # Writes
//
// Create is a conditional PutItem (attribute_not_exists(pk)). Put is a single
// UpdateItem that preserves created_* with if_not_exists and increments the
// version atomically. Update reads the document, merges the patch and writes
// it back conditioned on the item being unchanged since the read (version,
// created_at and updated_at, so a delete and re-create in between is noticed
// too), retrying on conflicts (optimistic concurrency), so concurrent updates
// never lose keys. UpdateIf and DeleteIf write once, conditioned on the
// revision read.
//
// The SDK sends a write again when its answer is lost (an HTTP 500, which
// DynamoDB documents as "may have succeeded", a dropped connection), and the
// condition of that attempt then fails on what the first one stored. Create
// and UpdateIf compare the item their condition failed on with the one they
// wrote (version, data, actor and updated_at, a docstore.Now() time no other
// write of the process repeats) and report their own write as done, not as a
// conflict.
//
// # Queries (limitation)
//
// DynamoDB cannot evaluate filters inside a JSON string attribute. Query
// reads the collection's items with a strongly consistent Query on pk —
// at most database.dynamodb.max_scan items (default 10000), in id order —
// and then filters, sorts and paginates in memory with the reference
// evaluator (docstore.Match / docstore.EvalQuery). A query also stops after
// reading 64 MiB of document data. Results are therefore exact for
// collections up to max_scan documents (and 64 MiB); for larger collections
// only the first documents by id are considered (a warning is logged).
// Each query costs read capacity proportional to the collection size.
//
// Documents are limited by DynamoDB's 400 KB item size; keep
// database.max_doc_bytes below ~390000.
package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/internal/dsutil"
)

const (
	defaultTable   = "backyard"
	defaultMaxScan = 10000
	countersSuffix = "#__collections"
	// maxUpdateAttempts bounds the optimistic retry loop of Update.
	maxUpdateAttempts = 100
	// batchSize is the BatchWriteItem limit.
	batchSize = 25
	// tableWait bounds how long Open waits for a new table to become ACTIVE.
	tableWait = 5 * time.Minute
	// maxScanBytes bounds the document bytes one query reads into memory
	// (alongside max_scan), so a collection of large documents cannot exhaust
	// the server's memory.
	maxScanBytes = 64 << 20
	// counterTimeout bounds a counter update, which runs even if the caller's
	// context is canceled right after the document write.
	counterTimeout = 10 * time.Second
)

// Attribute names.
const (
	attrPK        = "pk"
	attrSK        = "sk"
	attrData      = "data"
	attrCreatedAt = "created_at"
	attrUpdatedAt = "updated_at"
	attrCreatedBy = "created_by"
	attrUpdatedBy = "updated_by"
	attrVersion   = "version"
	attrCount     = "n"
)

// Store is a DynamoDB-backed document store.
type Store struct {
	client  *dynamodb.Client
	table   string
	maxScan int
}

var _ docstore.Store = (*Store)(nil)

// Open creates a DynamoDB client from the default AWS configuration chain
// (environment, shared config, IAM role…) with cfg.Region and cfg.Endpoint
// applied, then calls New. With a custom endpoint (DynamoDB Local) and no
// credentials available, static dummy credentials are used.
func Open(ctx context.Context, cfg config.DynamoDBConfig) (*Store, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("docstore/dynamodb: loading AWS configuration: %w", err)
	}
	if cfg.Endpoint != "" {
		if awsCfg.Region == "" {
			awsCfg.Region = "us-east-1"
		}
		cerr := errors.New("no credentials provider")
		if awsCfg.Credentials != nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, cerr = awsCfg.Credentials.Retrieve(cctx)
			cancel()
		}
		if cerr != nil {
			slog.Info("docstore/dynamodb: no AWS credentials found; using dummy credentials for custom endpoint", "endpoint", cfg.Endpoint)
			awsCfg.Credentials = credentials.NewStaticCredentialsProvider("local", "local", "")
		}
	}
	if awsCfg.Region == "" {
		return nil, errors.New("docstore/dynamodb: no AWS region configured (set database.dynamodb.region or AWS_REGION)")
	}
	client := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	return New(ctx, client, cfg)
}

// New builds a store on an existing client using cfg.Table (default
// "backyard") and cfg.MaxScan (default 10000). With cfg.CreateTable the table
// is created (on-demand billing) if missing, and New waits until it is ACTIVE.
func New(ctx context.Context, client *dynamodb.Client, cfg config.DynamoDBConfig) (*Store, error) {
	s := &Store{client: client, table: cfg.Table, maxScan: cfg.MaxScan}
	if s.table == "" {
		s.table = defaultTable
	}
	if s.maxScan <= 0 {
		s.maxScan = defaultMaxScan
	}
	if cfg.CreateTable {
		if err := s.ensureTable(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) ensureTable(ctx context.Context) error {
	_, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(s.table)})
	var nf *types.ResourceNotFoundException
	switch {
	case err == nil:
	case errors.As(err, &nf):
		_, err = s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:   aws.String(s.table),
			BillingMode: types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String(attrPK), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String(attrSK), AttributeType: types.ScalarAttributeTypeS},
			},
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String(attrPK), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String(attrSK), KeyType: types.KeyTypeRange},
			},
		})
		var inUse *types.ResourceInUseException
		if err != nil && !errors.As(err, &inUse) {
			return fmt.Errorf("docstore/dynamodb: creating table %q: %w", s.table, err)
		}
		slog.Info("docstore/dynamodb: created table", "table", s.table)
	default:
		return fmt.Errorf("docstore/dynamodb: describing table %q: %w", s.table, err)
	}
	w := dynamodb.NewTableExistsWaiter(s.client, func(o *dynamodb.TableExistsWaiterOptions) {
		o.MinDelay = 500 * time.Millisecond
		o.MaxDelay = 5 * time.Second
	})
	if err := w.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(s.table)}, tableWait); err != nil {
		return fmt.Errorf("docstore/dynamodb: waiting for table %q: %w", s.table, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// encoding

func partitionKey(site, collection string) string { return site + "#" + collection }

func countersKey(site string) string { return site + countersSuffix }

func attrS(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func attrN(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func key(site, collection, id string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{attrPK: attrS(partitionKey(site, collection)), attrSK: attrS(id)}
}

func docItem(d *docstore.Doc, raw []byte) map[string]types.AttributeValue {
	item := key(d.Site, d.Collection, d.ID)
	item[attrData] = attrS(string(raw))
	item[attrCreatedAt] = attrN(d.CreatedAt.UnixNano())
	item[attrUpdatedAt] = attrN(d.UpdatedAt.UnixNano())
	item[attrCreatedBy] = attrS(d.CreatedBy)
	item[attrUpdatedBy] = attrS(d.UpdatedBy)
	item[attrVersion] = attrN(d.Version)
	return item
}

func getS(item map[string]types.AttributeValue, name string) (string, bool) {
	v, ok := item[name].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

func getN(item map[string]types.AttributeValue, name string) (int64, bool) {
	v, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		// Counters and versions are integers; tolerate a decimal rendering.
		f, ferr := strconv.ParseFloat(v.Value, 64)
		if ferr != nil {
			return 0, false
		}
		n = int64(f)
	}
	return n, true
}

func decodeItem(item map[string]types.AttributeValue, site, collection string) (*docstore.Doc, error) {
	id, ok := getS(item, attrSK)
	if !ok {
		return nil, errors.New("docstore/dynamodb: item without sort key")
	}
	raw, _ := getS(item, attrData)
	data, err := dsutil.DecodeData([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("docstore/dynamodb: %s/%s/%s: %w", site, collection, id, err)
	}
	created, _ := getN(item, attrCreatedAt)
	updated, _ := getN(item, attrUpdatedAt)
	version, _ := getN(item, attrVersion)
	createdBy, _ := getS(item, attrCreatedBy)
	updatedBy, _ := getS(item, attrUpdatedBy)
	return &docstore.Doc{
		Site: site, Collection: collection, ID: id, Data: data,
		CreatedAt: time.Unix(0, created).UTC(), UpdatedAt: time.Unix(0, updated).UTC(),
		CreatedBy: createdBy, UpdatedBy: updatedBy, Version: version,
	}, nil
}

func isConditionFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

// conditionFailure reports whether err is a failed condition, and returns
// the item it was checked against when DynamoDB sent it (a write with
// ReturnValuesOnConditionCheckFailure ALL_OLD; empty when there is none).
//
// The SDK sends a write again when its answer is lost (an HTTP 500, which
// DynamoDB documents as "may have succeeded", a dropped connection, a
// timeout). When the first attempt was applied, the condition of the next
// one fails on that very write; sameItem tells it from another writer's.
func conditionFailure(err error) (map[string]types.AttributeValue, bool) {
	var ccf *types.ConditionalCheckFailedException
	if !errors.As(err, &ccf) {
		return nil, false
	}
	return ccf.Item, true
}

// sameItem reports whether a stored item holds the attributes of want, an
// item built by docItem, with the same values. Its updated_at is a docstore.Now()
// timestamp, which no other write of this process repeats, so with the
// version, data and actor it identifies one write.
func sameItem(item, want map[string]types.AttributeValue) bool {
	if len(item) == 0 {
		return false
	}
	for name, w := range want {
		switch w := w.(type) {
		case *types.AttributeValueMemberS:
			if v, ok := getS(item, name); !ok || v != w.Value {
				return false
			}
		case *types.AttributeValueMemberN:
			n, err := strconv.ParseInt(w.Value, 10, 64)
			if v, ok := getN(item, name); err != nil || !ok || v != n {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// apiError is the subset of smithy.APIError used to classify AWS errors.
type apiError interface {
	error
	ErrorCode() string
	ErrorMessage() string
}

// mapErr wraps an AWS error, translating item-size errors into ErrTooLarge.
func mapErr(op string, err error) error {
	var ae apiError
	if errors.As(err, &ae) && ae.ErrorCode() == "ValidationException" &&
		strings.Contains(strings.ToLower(ae.ErrorMessage()), "item size") {
		return fmt.Errorf("%w: %s", docstore.ErrTooLarge, ae.ErrorMessage())
	}
	return fmt.Errorf("docstore/dynamodb: %s: %w", op, err)
}

// ---------------------------------------------------------------------------
// Store

// Create inserts a new document; see docstore.Store.
func (s *Store) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if id == "" {
		id = docstore.NewID()
	}
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	body, raw, err := dsutil.PrepareWrite(data)
	if err != nil {
		return nil, err
	}
	now := docstore.Now()
	d := &docstore.Doc{
		Site: site, Collection: collection, ID: id, Data: body,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor, Version: 1,
	}
	item := docItem(d, raw)
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{
			"#pk": attrPK,
		},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if old, failed := conditionFailure(err); failed {
		// An attempt the SDK sent again fails on the item an earlier one
		// stored: that one is this write.
		if !sameItem(old, item) {
			return nil, fmt.Errorf("%w: document %q already exists in %s", docstore.ErrConflict, id, collection)
		}
		err = nil
	}
	if err != nil {
		return nil, mapErr("create", err)
	}
	s.bumpCounter(ctx, site, collection, 1)
	return d, nil
}

// bumpCounter adjusts a collection counter. Failures are logged, not
// returned: the document write already succeeded.
func (s *Store) bumpCounter(ctx context.Context, site, collection string, delta int64) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), counterTimeout)
	defer cancel()
	_, err := s.client.UpdateItem(cctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			attrPK: attrS(countersKey(site)),
			attrSK: attrS(collection),
		},
		UpdateExpression:          aws.String("ADD #n :d"),
		ExpressionAttributeNames:  map[string]string{"#n": attrCount},
		ExpressionAttributeValues: map[string]types.AttributeValue{":d": attrN(delta)},
	})
	if err != nil {
		slog.Warn("docstore/dynamodb: updating collection counter failed", "site", site, "collection", collection, "delta", delta, "err", err)
	}
}

func (s *Store) getItem(ctx context.Context, site, collection, id string) (*docstore.Doc, error) {
	d, _, err := s.getRawItem(ctx, site, collection, id)
	return d, err
}

// getRawItem reads a document and also returns its item as stored.
func (s *Store) getRawItem(ctx context.Context, site, collection, id string) (*docstore.Doc, map[string]types.AttributeValue, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            key(site, collection, id),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, nil, mapErr("get", err)
	}
	if len(out.Item) == 0 {
		return nil, nil, docstore.ErrNotFound
	}
	d, err := decodeItem(out.Item, site, collection)
	return d, out.Item, err
}

// unchangedCondition returns a condition expression that holds only while the
// document is exactly the item that was read. The version alone is not
// enough: Create, and Put on a missing item, start over at version 1, so a
// delete and re-create between the read and the write would pass a version
// check and the write would bring back the deleted document. created_at
// identifies the incarnation and updated_at its last write; both are compared
// as stored.
func unchangedCondition(item map[string]types.AttributeValue) (string, map[string]string, map[string]types.AttributeValue) {
	names := make(map[string]string, 3)
	values := make(map[string]types.AttributeValue, 3)
	conds := make([]string, 0, 3)
	for i, attr := range []string{attrVersion, attrCreatedAt, attrUpdatedAt} {
		name, value := "#c"+strconv.Itoa(i), ":c"+strconv.Itoa(i)
		names[name] = attr
		if v, ok := item[attr]; ok {
			values[value] = v
			conds = append(conds, name+" = "+value)
		} else {
			conds = append(conds, "attribute_not_exists("+name+")")
		}
	}
	if len(values) == 0 {
		values = nil // DynamoDB rejects an empty map
	}
	return strings.Join(conds, " AND "), names, values
}

// Get returns a document (strongly consistent read); see docstore.Store.
func (s *Store) Get(ctx context.Context, site, collection, id string) (*docstore.Doc, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return nil, err
	}
	return s.getItem(ctx, site, collection, id)
}

// Put creates or replaces a document in one UpdateItem call; see
// docstore.Store.
func (s *Store) Put(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	body, raw, err := dsutil.PrepareWrite(data)
	if err != nil {
		return nil, err
	}
	now := docstore.Now()
	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key:       key(site, collection, id),
		UpdateExpression: aws.String("SET #d = :d, #ua = :now, #ub = :actor, " +
			"#ca = if_not_exists(#ca, :now), #cb = if_not_exists(#cb, :actor) ADD #v :one"),
		ExpressionAttributeNames: map[string]string{
			"#d": attrData, "#ua": attrUpdatedAt, "#ub": attrUpdatedBy,
			"#ca": attrCreatedAt, "#cb": attrCreatedBy, "#v": attrVersion,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":d": attrS(string(raw)), ":now": attrN(now.UnixNano()), ":actor": attrS(actor), ":one": attrN(1),
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		return nil, mapErr("put", err)
	}
	d, err := decodeItem(out.Attributes, site, collection)
	if err != nil {
		return nil, err
	}
	d.Data = body
	if d.Version == 1 {
		s.bumpCounter(ctx, site, collection, 1)
	}
	return d, nil
}

// Update shallow-merges patch into an existing document using optimistic
// concurrency (a write conditioned on the item read being unchanged); see
// docstore.Store.
func (s *Store) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	p, err := dsutil.PreparePatch(patch)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		cur, item, err := s.getRawItem(ctx, site, collection, id)
		if err != nil {
			return nil, err
		}
		merged, raw, err := dsutil.MergePatch(cur.Data, p)
		if err != nil {
			return nil, err
		}
		next := *cur
		next.Data = merged
		next.UpdatedAt = docstore.Now()
		next.UpdatedBy = actor
		next.Version = cur.Version + 1
		cond, names, values := unchangedCondition(item)
		_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:                 aws.String(s.table),
			Item:                      docItem(&next, raw),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		})
		if err == nil {
			return &next, nil
		}
		if !isConditionFailed(err) {
			return nil, mapErr("update", err)
		}
		// Lost a race with another writer (or the document was deleted, and
		// maybe created again): back off with jitter and re-read.
		if attempt+1 >= maxUpdateAttempts {
			return nil, fmt.Errorf("%w: document %q is updated too frequently", docstore.ErrConflict, id)
		}
		backoff := time.Duration(rand.Int64N(int64(min(time.Millisecond<<min(attempt, 6), 50*time.Millisecond)) + 1))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// UpdateIf is Update while the document is at rev: the item read must be at
// rev, and the write is conditioned on it being unchanged; see
// docstore.Store.
func (s *Store) UpdateIf(ctx context.Context, site, collection, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	p, err := dsutil.PreparePatch(patch)
	if err != nil {
		return nil, err
	}
	changed := fmt.Errorf("%w: document %q has changed", docstore.ErrConflict, id)
	cur, item, err := s.getRawItem(ctx, site, collection, id)
	if err != nil {
		return nil, err
	}
	if !cur.At(rev) {
		return nil, changed
	}
	merged, raw, err := dsutil.MergePatch(cur.Data, p)
	if err != nil {
		return nil, err
	}
	next := *cur
	next.Data = merged
	next.UpdatedAt = docstore.Now()
	next.UpdatedBy = actor
	next.Version = cur.Version + 1
	want := docItem(&next, raw)
	cond, names, values := unchangedCondition(item)
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                           aws.String(s.table),
		Item:                                want,
		ConditionExpression:                 aws.String(cond),
		ExpressionAttributeNames:            names,
		ExpressionAttributeValues:           values,
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err == nil {
		return &next, nil
	}
	old, failed := conditionFailure(err)
	if !failed {
		return nil, mapErr("update", err)
	}
	// Deleted since the read (or the item not sent): read it again.
	if len(old) == 0 {
		if _, old, err = s.getRawItem(ctx, site, collection, id); err != nil {
			return nil, err
		}
	}
	// An attempt the SDK sent again fails on this write, which an earlier
	// one applied; anything else is another writer's.
	if sameItem(old, want) {
		return &next, nil
	}
	return nil, changed
}

// Delete removes a document; see docstore.Store.
func (s *Store) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	out, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:    aws.String(s.table),
		Key:          key(site, collection, id),
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return false, mapErr("delete", err)
	}
	if len(out.Attributes) == 0 {
		return false, nil
	}
	s.bumpCounter(ctx, site, collection, -1)
	return true, nil
}

// DeleteIf removes a document while it is at rev, with one conditional
// DeleteItem; see docstore.Store.
func (s *Store) DeleteIf(ctx context.Context, site, collection, id string, rev docstore.Rev) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                aws.String(s.table),
		Key:                      key(site, collection, id),
		ConditionExpression:      aws.String("#v = :v AND #ca = :ca"),
		ExpressionAttributeNames: map[string]string{"#v": attrVersion, "#ca": attrCreatedAt},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":v": attrN(rev.Version), ":ca": attrN(docstore.TruncTime(rev.CreatedAt).UnixNano()),
		},
	})
	if isConditionFailed(err) {
		// At another revision, or gone. Gone after an attempt the SDK sent
		// again may be this delete, applied by an earlier attempt, or another
		// writer's (who adjusted the counter): it is reported as not removed
		// and the counter left alone, so it is never counted twice. A lost
		// answer can leave the counter one too high, as a crash between the
		// two calls can (see the package documentation).
		return false, nil
	}
	if err != nil {
		return false, mapErr("delete", err)
	}
	s.bumpCounter(ctx, site, collection, -1)
	return true, nil
}

// Query reads up to max_scan documents of the collection and evaluates the
// query in memory; see the package documentation.
func (s *Store) Query(ctx context.Context, site, collection string, q docstore.Query) (*docstore.Page, error) {
	if err := dsutil.CheckNamespace(site, collection); err != nil {
		return nil, err
	}
	q, err := docstore.PrepareQuery(q)
	if err != nil {
		return nil, err
	}
	matched := make([]*docstore.Doc, 0, 64)
	pager := dynamodb.NewQueryPaginator(s.client, &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		KeyConditionExpression:    aws.String("#pk = :pk"),
		ExpressionAttributeNames:  map[string]string{"#pk": attrPK},
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(partitionKey(site, collection))},
		ConsistentRead:            aws.Bool(true),
	})
	scanned, scannedBytes, truncated := 0, 0, false
scan:
	for pager.HasMorePages() {
		out, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapErr("query", err)
		}
		for _, item := range out.Items {
			if scanned >= s.maxScan || scannedBytes >= maxScanBytes {
				truncated = true
				break scan
			}
			scanned++
			if raw, ok := getS(item, attrData); ok {
				scannedBytes += len(raw)
			}
			d, err := decodeItem(item, site, collection)
			if err != nil {
				return nil, err
			}
			// Filter while scanning so only matching documents are kept.
			if docstore.Match(d, q.Where) {
				matched = append(matched, d)
			}
		}
	}
	if truncated {
		slog.Warn("docstore/dynamodb: query truncated; results only consider the first documents by id",
			"site", site, "collection", collection, "scanned", scanned, "scanned_bytes", scannedBytes,
			"max_scan", s.maxScan, "max_scan_bytes", maxScanBytes)
	}
	q.Where = nil // already applied
	return docstore.EvalQuery(matched, q), nil
}

type counter struct {
	collection string
	n          int64
}

// counters returns all collection counter items of a site.
func (s *Store) counters(ctx context.Context, site string) ([]counter, error) {
	var out []counter
	pager := dynamodb.NewQueryPaginator(s.client, &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		KeyConditionExpression:    aws.String("#pk = :pk"),
		ExpressionAttributeNames:  map[string]string{"#pk": attrPK},
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(countersKey(site))},
		ConsistentRead:            aws.Bool(true),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapErr("list collections", err)
		}
		for _, item := range page.Items {
			c, _ := getS(item, attrSK)
			n, _ := getN(item, attrCount)
			out = append(out, counter{collection: c, n: n})
		}
	}
	return out, nil
}

// Collections lists collections with a positive counter, sorted bytewise.
func (s *Store) Collections(ctx context.Context, site string) ([]string, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return nil, err
	}
	cs, err := s.counters(ctx, site)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, c := range cs {
		if c.n > 0 {
			out = append(out, c.collection)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Count sums the collection counters of a site.
func (s *Store) Count(ctx context.Context, site string) (int64, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return 0, err
	}
	cs, err := s.counters(ctx, site)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, c := range cs {
		if c.n > 0 {
			n += c.n
		}
	}
	return n, nil
}

// DeleteSite deletes every document of every collection of the site, then
// the site's counters.
func (s *Store) DeleteSite(ctx context.Context, site string) error {
	if err := docstore.ValidateSite(site); err != nil {
		return err
	}
	cs, err := s.counters(ctx, site)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := docstore.ValidateCollection(c.collection, strings.HasPrefix(c.collection, "_")); err != nil {
			continue // not ours; never derive a partition from a bad name
		}
		pager := dynamodb.NewQueryPaginator(s.client, &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			KeyConditionExpression:    aws.String("#pk = :pk"),
			ProjectionExpression:      aws.String("#pk, #sk"),
			ExpressionAttributeNames:  map[string]string{"#pk": attrPK, "#sk": attrSK},
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(partitionKey(site, c.collection))},
			ConsistentRead:            aws.Bool(true),
		})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return mapErr("delete site", err)
			}
			if err := s.batchDelete(ctx, page.Items); err != nil {
				return err
			}
		}
	}
	keys := make([]map[string]types.AttributeValue, 0, len(cs))
	for _, c := range cs {
		keys = append(keys, map[string]types.AttributeValue{attrPK: attrS(countersKey(site)), attrSK: attrS(c.collection)})
	}
	return s.batchDelete(ctx, keys)
}

// batchDelete deletes items by key in batches of 25, retrying unprocessed
// items with backoff.
func (s *Store) batchDelete(ctx context.Context, items []map[string]types.AttributeValue) error {
	for chunk := range slices.Chunk(items, batchSize) {
		reqs := make([]types.WriteRequest, 0, len(chunk))
		for _, it := range chunk {
			reqs = append(reqs, types.WriteRequest{DeleteRequest: &types.DeleteRequest{
				Key: map[string]types.AttributeValue{attrPK: it[attrPK], attrSK: it[attrSK]},
			}})
		}
		pending := map[string][]types.WriteRequest{s.table: reqs}
		for attempt := 0; len(pending[s.table]) > 0; attempt++ {
			if attempt > 0 {
				if attempt > 10 {
					return fmt.Errorf("docstore/dynamodb: delete site: %d items still unprocessed", len(pending[s.table]))
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(min(50<<attempt, 5000)) * time.Millisecond):
				}
			}
			out, err := s.client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
			if err != nil {
				return mapErr("delete site", err)
			}
			pending = out.UnprocessedItems
		}
	}
	return nil
}

// Ping checks that the table is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(s.table)}); err != nil {
		return fmt.Errorf("docstore/dynamodb: ping: %w", err)
	}
	return nil
}

// Close is a no-op (the AWS client holds no resources that need closing).
func (s *Store) Close() error { return nil }
