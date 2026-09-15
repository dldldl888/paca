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
function visit(node, prefix = []) {
  if (ts.isCallExpression(node) && node.arguments[0] && ts.isStringLiteral(node.arguments[0])) {
    const kind = callName(node.expression);
    const callback = node.arguments.find(a => ts.isArrowFunction(a) || ts.isFunctionExpression(a));
    const fullName = [...prefix, node.arguments[0].text];
    if (kind === 'describe' && callback) { visit(callback.body, fullName); return; }
    if (['test', 'it'].includes(kind) && fullName.join(' ') === name) {
      let called = false, asserted = false;
      function inspect(part) {
        if (ts.isFunctionDeclaration(part)) return;
        if (ts.isArrowFunction(part) || ts.isFunctionExpression(part)) {
          let expression = part;
          while (ts.isParenthesizedExpression(expression.parent)) expression = expression.parent;
          let invocation = expression.parent;
          let runs = ts.isCallExpression(invocation) && invocation.expression === expression;
          if (ts.isCallExpression(invocation) && callName(invocation.expression) === 'expect') {
            while (ts.isPropertyAccessExpression(invocation.parent)) invocation = invocation.parent;
            runs ||= ['toThrow', 'toThrowError'].includes(callName(invocation)) && ts.isCallExpression(invocation.parent);
          }
          if (!runs) return;
        }
        if (ts.isCallExpression(part)) {
          called ||= callName(part.expression) === sut;
          asserted ||= callName(part.expression) === 'expect';
        }
        ts.forEachChild(part, inspect);
      }
      if (callback) inspect(callback.body);
      valid ||= called && asserted;
    }
  }
  ts.forEachChild(node, child => visit(child, prefix));
}
visit(source);
if (!valid) { console.error('missing test body, SUT call or behavior assertion'); process.exit(1); }
