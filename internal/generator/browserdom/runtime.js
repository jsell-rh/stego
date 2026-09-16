// These functions accept trusted application render output, never user HTML.
// They are not HTML or CSS sanitizers and do not change native DOM methods.
const maxHTML = 1024 * 1024;
const maxNodes = 16384;

export function trustedFragment(ownerDocument, value) {
  if (typeof value !== 'string' || value.length > maxHTML) {
    throw new TypeError('Invalid trusted render input');
  }
  const template = ownerDocument.createElement('template');
  template.innerHTML = value;
  const fragment = template.content;
  const walker = ownerDocument.createTreeWalker(fragment, 0xFFFFFFFF);
  let count = 0;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (++count > maxNodes) throw new RangeError('Trusted render node limit');
    if (node.nodeType !== 1) continue;
    if (['script', 'style', 'template', 'iframe', 'object', 'embed', 'link', 'meta', 'base'].includes(node.localName)) {
      throw new TypeError('Unsupported trusted render element');
    }
    for (const attribute of node.attributes) {
      if (/^on/i.test(attribute.name) || attribute.name === 'nonce') {
        throw new TypeError('Unsupported trusted render attribute');
      }
    }
    if (node.hasAttribute('style')) {
      const css = node.getAttribute('style');
      node.removeAttribute('style');
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
