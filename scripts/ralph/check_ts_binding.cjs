// Use the package's existing TypeScript compiler, not a file-wide string match.
const path = require('node:path');
const fs = require('node:fs');
const ts = require(path.join(process.cwd(), 'node_modules/typescript'));
const [file, name, sut] = process.argv.slice(2);
const source = ts.createSourceFile(file, fs.readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true);
let valid = false;
function callName(node) {
  return ts.isIdentifier(node) ? node.text : ts.isPropertyAccessExpression(node) ? node.name.text : '';
}
function visit(node) {
  if (ts.isCallExpression(node) && ['test', 'it'].includes(callName(node.expression)) && ts.isStringLiteral(node.arguments[0]) && node.arguments[0].text === name) {
    const callback = node.arguments.find(a => ts.isArrowFunction(a) || ts.isFunctionExpression(a));
    let called = false, asserted = false;
    function inspect(part) {
      if (ts.isCallExpression(part)) {
        called ||= callName(part.expression) === sut;
        asserted ||= callName(part.expression) === 'expect';
      }
      ts.forEachChild(part, inspect);
    }
    if (callback) inspect(callback.body);
    valid ||= called && asserted;
  }
  ts.forEachChild(node, visit);
}
visit(source);
if (!valid) { console.error('missing test body, SUT call or behavior assertion'); process.exit(1); }
