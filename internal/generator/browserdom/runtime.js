// These functions accept trusted application render output, never user HTML.
// They are not HTML or CSS sanitizers and do not change native DOM methods.
const maxHTML = 1024 * 1024;
const maxNodes = 16384;
const storedStyle = 'data-stego-render-style';
const forbiddenTags = new Set(['script', 'style', 'template', 'iframe', 'object', 'embed', 'link', 'meta', 'base', 'textarea', 'title', 'noscript', 'xmp', 'plaintext', 'noembed', 'noframes']);

// Accept the explicit attribute grammar used by trusted renderers. The native
// HTML parser still owns entities, namespaces, and tree construction. Renaming
// occurs before parsing because even inert style attributes can violate CSP.
function deferStyles(value) {
  const output = [];
  let copied = 0;
  let at = 0;
  const fail = () => { throw new TypeError('Unsupported trusted render syntax'); };
  const space = character => character !== undefined && /[\t\n\f\r ]/.test(character);
  const name = () => {
    const start = at;
    while (at < value.length && /[A-Za-z0-9_:.-]/.test(value[at])) at++;
    if (at === start) fail();
    return value.slice(start, at).toLowerCase();
  };
  while (at < value.length) {
    if (value[at++] !== '<') continue;
    if (value.startsWith('!--', at)) {
      const end = value.indexOf('-->', at + 3);
      if (end < 0) fail();
      const comment = value.slice(at + 3, end);
      if (comment.startsWith('>') || comment.startsWith('->') || comment.includes('--') || comment.endsWith('-')) fail();
      at = end + 3;
      continue;
    }
    let closing = false;
    if (value[at] === '/') { closing = true; at++; }
    if (!/[A-Za-z]/.test(value[at] ?? '')) fail();
    const tag = name();
    if (forbiddenTags.has(tag)) fail();
    const attributes = new Set();
    for (;;) {
      const separated = space(value[at]);
      while (space(value[at])) at++;
      if (value[at] === '>') { at++; break; }
      if (value[at] === '/' && value[at + 1] === '>' && !closing) { at += 2; break; }
      if (closing || !separated || at >= value.length) fail();
      const start = at;
      const attribute = name();
      if (attributes.has(attribute) || attribute === storedStyle || attribute === 'nonce' || attribute.startsWith('on')) fail();
      attributes.add(attribute);
      if (attribute === 'style') {
        output.push(value.slice(copied, start), storedStyle);
        copied = at;
      }
      const afterName = at;
      while (space(value[at])) at++;
      if (value[at] !== '=') {
        at = afterName;
        continue;
      }
      at++;
      while (space(value[at])) at++;
      const quote = value[at++];
      if (quote !== '"' && quote !== "'") fail();
      const end = value.indexOf(quote, at);
      if (end < 0) fail();
      at = end + 1;
    }
  }
  output.push(value.slice(copied));
  return output.join('');
}

export function trustedFragment(ownerDocument, value) {
  const nativeHTML = ownerDocument.defaultView?.trustedTypes?.isHTML(value) === true;
  if ((typeof value !== 'string' && !nativeHTML) || String(value).length > maxHTML) {
    throw new TypeError('Invalid trusted render input');
  }
  const template = ownerDocument.createElement('template');
  template.innerHTML = deferStyles(String(value));
  const fragment = template.content;
  const walker = ownerDocument.createTreeWalker(fragment, 0xFFFFFFFF);
  let count = 0;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (++count > maxNodes) throw new RangeError('Trusted render node limit');
    if (node.nodeType !== 1) continue;
    if (forbiddenTags.has(node.localName)) {
      throw new TypeError('Unsupported trusted render element');
    }
    for (const attribute of node.attributes) {
      if (/^on/i.test(attribute.name) || attribute.name === 'nonce') {
        throw new TypeError('Unsupported trusted render attribute');
      }
    }
    if (node.hasAttribute(storedStyle)) {
      const css = node.getAttribute(storedStyle);
      node.removeAttribute(storedStyle);
      node.style.cssText = css;
    }
  }
  return fragment;
}

export function replaceTrustedHTML(target, value) {
  const fragment = trustedFragment(target.ownerDocument, value);
  target.replaceChildren(fragment);
}

export function appendTrustedHTML(target, value) {
  const fragment = trustedFragment(target.ownerDocument, value);
  target.append(fragment);
}

export function createStyleElement(ownerDocument) {
  const values = ownerDocument.querySelectorAll('meta[name="stego-style-nonce"]');
  if (values.length !== 1 || !/^[A-Za-z0-9_-]{43}$/.test(values[0].content)) {
    throw new TypeError('Missing document style nonce');
  }
  const style = ownerDocument.createElement('style');
  style.setAttribute('nonce', values[0].content);
  return style;
}
