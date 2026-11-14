// Package unsharedocument is the fill of the command unshare-document of the test service: it removes the
// user's viewer relation of the document, through the typed relation helper of the command contract
// (ST-SUP-RC32-OC13).
package unsharedocument

import (
	"context"

	commands "example.com/shop/out/commands"
)

// New gives the fill of the command unshare-document.
func New() commands.UnshareDocumentFill {
	return func(ctx context.Context, request *commands.UnshareDocumentRequest, h *commands.UnshareDocumentHandle) (*commands.UnshareDocumentResponse, error) {
		documents, err := commands.DocumentRepository(h)
		if err != nil {
			return nil, err
		}
		document, err := documents.Get(ctx, request.DocumentId)
		if err != nil {
			return nil, err
		}
		if err := commands.UnshareDocumentUnrelateViewer(ctx, h, document, "user", request.UserId); err != nil {
			return nil, err
		}
		return &commands.UnshareDocumentResponse{DocumentId: document.ID()}, nil
	}
}
