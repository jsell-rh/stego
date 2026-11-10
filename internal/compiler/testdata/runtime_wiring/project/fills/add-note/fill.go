// Package addnote is the fill of the command add-note of the test service: it writes one Note of the order.
package addnote

import (
	"context"

	commands "example.com/shop/out/commands"
)

// New gives the fill of the command add-note.
func New() commands.AddNoteFill {
	return func(ctx context.Context, request *commands.AddNoteRequest, h *commands.AddNoteHandle) (*commands.AddNoteResponse, error) {
		notes, err := commands.NoteRepository(h)
		if err != nil {
			return nil, err
		}
		note, err := notes.Create(ctx, commands.Note{OrderID: request.OrderId, Text: request.Text}, nil)
		if err != nil {
			return nil, err
		}
		return &commands.AddNoteResponse{NoteId: note.ID()}, nil
	}
}
