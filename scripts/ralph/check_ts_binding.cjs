// Use the repository's existing TypeScript compiler, not a file-wide string match.
const path = require('node:path');
const fs = require('node:fs');
const ts = require(require.resolve('typescript', { paths: [process.cwd(), path.join(__dirname, '../../apps/web')] }));
const [file, name, sut, runner = 'bun'] = process.argv.slice(2);
const source = ts.createSourceFile(file, fs.readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true);
const imports = new Map();
for (const statement of source.statements) {
  if (!ts.isImportDeclaration(statement) || !statement.importClause || statement.importClause.isTypeOnly) continue;
  const module = statement.moduleSpecifier.text;
  if (statement.importClause.name) imports.set(statement.importClause.name.text, module);
  const bindings = statement.importClause.namedBindings;
  if (bindings && ts.isNamedImports(bindings)) {
    for (const element of bindings.elements) if (!element.isTypeOnly) imports.set(element.name.text, module);
  }
}
const mocked = new Set();
function collectMocks(node) {
  if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression) &&
      ['vi', 'jest'].includes(callName(node.expression.expression)) && node.expression.name.text === 'mock' &&
      node.arguments[0] && ts.isStringLiteral(node.arguments[0])) mocked.add(node.arguments[0].text);
  ts.forEachChild(node, collectMocks);
}
let valid = false;
function callName(node) {
  return ts.isIdentifier(node) ? node.text : ts.isPropertyAccessExpression(node) ? node.name.text : '';
}
collectMocks(source);
function testKind(node) {
  if (ts.isPropertyAccessExpression(node) && ['skip', 'only', 'todo', 'concurrent'].includes(node.name.text)) {
    return testKind(node.expression);
  }
  return callName(node);
}
function rendered(node) {
  let child = node;
  for (let parent = node.parent; parent; child = parent, parent = parent.parent) {
    if (ts.isCallExpression(parent) && parent.arguments.includes(child) && callName(parent.expression).startsWith('render')) return true;
    if (ts.isFunctionLike(parent)) return false;
  }
  return false;
}
function isAssertion(node) {
  if (!ts.isCallExpression(node) || !ts.isPropertyAccessExpression(node.expression)) return false;
  let target = node.expression.expression;
  while (ts.isPropertyAccessExpression(target)) target = target.expression;
  return ts.isCallExpression(target) && callName(target.expression) === 'expect';
}
function visit(node, prefix = []) {
  if (ts.isCallExpression(node) && node.arguments[0] && ts.isStringLiteral(node.arguments[0])) {
    const kind = testKind(node.expression);
    const callback = node.arguments.find(a => ts.isArrowFunction(a) || ts.isFunctionExpression(a));
    const fullName = [...prefix, node.arguments[0].text];
    if (kind === 'describe' && callback) { visit(callback.body, fullName); return; }
    if (['test', 'it'].includes(kind) && fullName.join(' ') === name) {
      let bound = false, asserted = false;
      function inspect(part) {
        if (ts.isFunctionDeclaration(part)) return;
        if (ts.isIfStatement(part) && part.expression.kind === ts.SyntaxKind.FalseKeyword) {
          if (part.elseStatement) inspect(part.elseStatement);
          return;
        }
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
          bound ||= callName(part.expression) === sut;
          asserted ||= isAssertion(part);
        }
        if (ts.isNewExpression(part)) bound ||= callName(part.expression) === sut;
        if (ts.isJsxOpeningElement(part) || ts.isJsxSelfClosingElement(part)) {
          bound ||= ts.isIdentifier(part.tagName) && part.tagName.text === sut && rendered(part);
        }
        ts.forEachChild(part, inspect);
      }
      if (callback) inspect(callback.body);
      valid ||= bound && asserted && (runner === 'bun' || (imports.has(sut) && !mocked.has(imports.get(sut))));
    }
  }
  ts.forEachChild(node, child => visit(child, prefix));
}
visit(source);
if (!valid) { console.error('missing test body, SUT call or behavior assertion'); process.exit(1); }
