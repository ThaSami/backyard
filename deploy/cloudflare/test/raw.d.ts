// Vite's ?raw imports (the file's text as a string), e.g. the shared
// querycases.json, which must be parsed with JSON.parse (see querycases.test.ts).
declare module "*?raw" {
  const text: string;
  export default text;
}
