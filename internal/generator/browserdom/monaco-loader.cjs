// Adapt reviewed Monaco render operations to the common STEGO DOM API.
// The original dependency remains subject to its own license and lock file.
const {createHash} = require('node:crypto');
const patches = require('./monaco-patches.json');
const prelude = 'import {createStyleElement as stegoCreateStyleElement, replaceTrustedHTML as stegoReplaceTrustedHTML, appendTrustedHTML as stegoAppendTrustedHTML} from "@stego/browser-dom";\n';

function transform(name, source) {
  const patch = patches.files[name];
  if (!Object.hasOwn(patches.files, name)) return source;
  if (typeof source !== 'string' || source.length > 4 * 1024 * 1024 || createHash('sha256').update(source).digest('hex') !== patch.sha256) {
    throw new Error(`STEGO requires the reviewed Monaco ${patches.version} source for ${name}`);
  }
  let output = source;
  for (const {before, after, count} of patch.replacements) {
    if (output.split(before).length - 1 !== count) throw new Error(`STEGO Monaco render operation differs in ${name}`);
    output = output.split(before).join(after);
  }
  return prelude + output;
}

module.exports = function (source) {
  this.cacheable(true);
  const marker = '/node_modules/monaco-editor/esm/';
  const resource = this.resourcePath.replaceAll('\\', '/');
  const at = resource.lastIndexOf(marker);
  if (at < 0) throw new Error('STEGO Monaco loader requires an ESM dependency file');
  return transform(resource.slice(at + marker.length), source);
};
module.exports.transform = transform;
