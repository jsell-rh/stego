package command

import (
	"strings"
	"testing"
)

func TestCommandRouteForm(t *testing.T) {
	t.Run("ST-SUP-RC32-OC11-T02 A command route outside the path form is refused before any file is written", func(t *testing.T) {
		for _, route := range []string{
			"POST /x\nvar\n_=func()int{panic(1)}()\n//",
			"POST /orders/{orderId}/reservations;x",
			"POST /orders/{orderId}/reservations\r",
			"POST /orders/{orderId}/",
			"POST /orders/{order}id/reservations",
			"POST /orders/{orderId}/reser`vations",
		} {
			svc := declaration(t)
			svc.Commands[0].Route = route
			files, _, err := new(Generator).Generate(commandContext(t, svc, "command.command"))
			if err == nil || files != nil || !strings.Contains(err.Error(), "a command route is POST and a path") {
				t.Errorf("the route %q gave %v with %d files, want a refusal", route, err, len(files))
			}
		}
		generated(t, commandContext(t, declaration(t), "command.command"))
	})
}
