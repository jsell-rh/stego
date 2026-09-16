/** Accept trusted render output only. These functions are not sanitizers. */
export declare function trustedFragment(ownerDocument: Document, value: unknown): DocumentFragment;
export declare function replaceTrustedHTML(target: Element, value: unknown): void;
export declare function appendTrustedHTML(target: Element, value: unknown): void;
/** Create a style element with the current generated document's nonce. */
export declare function createStyleElement(ownerDocument: Document): HTMLStyleElement;
