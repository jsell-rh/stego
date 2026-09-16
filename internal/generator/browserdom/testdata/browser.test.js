import {replaceTrustedHTML, appendTrustedHTML, createStyleElement} from '/runtime.js';

const violations = [];
document.addEventListener('securitypolicyviolation', event => violations.push(event.effectiveDirective));
const settle = () => new Promise(resolve => setTimeout(resolve, 100));
const results = [];
function check(value, label) { if (!value) throw new Error(label); }
async function test(name, action) {
  violations.length = 0;
  try { await action(); results.push({name, passed: true}); }
  catch (error) { results.push({name, passed: false, error: error.message}); }
}
const target = document.createElement('div');
document.body.append(target);

await test('trusted fragment preserves layout and SVG without policy violations', async () => {
  replaceTrustedHTML(target, '<div class="line" style="position:relative;left:17px;height:29px;color:rgb(10, 20, 30)">first &lt;line&gt;</div><svg width="10" height="10" style="position:relative;left:7px"><path d="M0 0L10 10"/></svg>');
  await settle();
  check(target.children.length === 2, 'Render added extra elements');
  check(target.firstChild.textContent === 'first <line>', 'Text changed');
  const line = getComputedStyle(target.firstChild);
  check(line.left === '17px' && line.height === '29px' && line.color === 'rgb(10, 20, 30)', 'HTML layout differs');
  check(getComputedStyle(target.lastChild).left === '7px', 'SVG layout differs');
  check(violations.length === 0, `Policy violations: ${violations.join(',')}`);
});
await test('incremental append and replacement retain exact element counts', async () => {
  appendTrustedHTML(target, '<span style="display:block;height:31px">next</span>');
  check(target.children.length === 3 && getComputedStyle(target.lastChild).height === '31px', 'Append differs');
  replaceTrustedHTML(target, '<span style="display:block;height:11px">replacement</span>');
  await settle();
  check(target.children.length === 1 && getComputedStyle(target.firstChild).height === '11px', 'Replacement differs');
  check(violations.length === 0, 'Render violated policy');
});
await test('invalid input leaves the live target unchanged', async () => {
  for (const value of ['x'.repeat(1024 * 1024 + 1), '<i></i>'.repeat(16385), '<script>throw 1</script>', '<template><b>nested</b></template>', '<div onclick="throw 1"></div>']) {
    let failed = false;
    try { replaceTrustedHTML(target, value); } catch { failed = true; }
    check(failed && target.textContent === 'replacement', 'Invalid render changed target');
  }
  await settle();
  check(violations.length === 0, 'Rejected render caused a policy violation');
});
await test('nonced stylesheet and CSSOM updates apply', async () => {
  const style = createStyleElement(document);
  style.textContent = '.sheet-probe { height: 23px; }';
  document.head.append(style);
  const node = document.createElement('div'); node.className = 'sheet-probe'; target.replaceChildren(node);
  check(getComputedStyle(node).height === '23px', 'Nonced stylesheet blocked');
  style.sheet.insertRule('.sheet-probe { width: 41px; }', style.sheet.cssRules.length);
  check(getComputedStyle(node).width === '41px', 'CSSOM update blocked');
  await settle();
  check(violations.length === 0, 'Trusted stylesheet violated policy');
  style.remove();
});
await test('raw inline styles remain blocked', async () => {
  target.innerHTML = '<div style="height:123px">blocked</div>';
  const style = document.createElement('style'); style.textContent = 'body { padding:123px; }'; document.head.append(style);
  await settle();
  check(getComputedStyle(target.firstChild).height !== '123px', 'Raw style attribute applied');
  check(getComputedStyle(document.body).paddingTop !== '123px', 'Raw stylesheet applied');
  check(violations.includes('style-src-attr') && violations.includes('style-src-elem'), 'Policy did not reject raw styles');
  style.remove();
});
await fetch('/result', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({results, passed:results.every(result=>result.passed)})});
