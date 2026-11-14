// Package sharedocument is the fill of the command share-document of the test service: it relates the user
// to the document as a viewer, through the typed relation helper of the command contract (ST-SUP-RC32-OC13).
package sharedocument

import (
	"context"

	commands "example.com/shop/out/commands"
)

// New gives the fill of the command share-document.
func New() commands.ShareDocumentFill {
	return func(ctx context.Context, request *commands.ShareDocumentRequest, h *commands.ShareDocumentHandle) (*commands.ShareDocumentResponse, error) {
		documents, err := commands.DocumentRepository(h)
		if err != nil {
			return nil, err
		}
		document, err := documents.Get(ctx, request.DocumentId)
		if err != nil {
			return nil, err
		}
		if err := commands.ShareDocumentRelateViewer(ctx, h, document, "user", request.UserId); err != nil {
			return nil, err
		}
		return &commands.ShareDocumentResponse{DocumentId: document.ID()}, nil
	}
}
