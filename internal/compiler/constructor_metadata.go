package compiler

import (
	"fmt"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
)

// Validate all constructor references before unused constructors are removed.
// A bad reference must not silently remove middleware, cleanup, or dependency
// metadata. Map indexes are checked in order for stable diagnostics.
func validateConstructorMetadata(wirings []ComponentWiring) error {
	if err := validateDatabaseOpener(wirings); err != nil {
		return err
	}
	primarySeen := false
	primaryName := ""
	loggerSeen, loggerName := false, ""
	for _, component := range wirings {
		w := component.Wiring
		if w == nil {
			continue
		}
		check := func(index int, role string) error {
			if index < 0 || index >= len(w.Constructors) {
				return fmt.Errorf("component %q has invalid %s constructor index %d", component.Name, role, index)
			}
			return nil
		}
		if w.HTTPErrorLogger != nil {
			if err := check(*w.HTTPErrorLogger, "HTTP error logger"); err != nil {
				return err
			}
			if loggerSeen {
				return fmt.Errorf("components %q and %q both declare an HTTP error logger", loggerName, component.Name)
			}
			loggerSeen, loggerName = true, component.Name
		}
		for _, index := range constructorMetadataIndexes(w.ConstructorResources) {
			if err := check(index, "resource"); err != nil {
				return err
			}
			for _, resource := range w.ConstructorResources[index] {
				if resource != gen.ServiceContext && resource != gen.SQLDatabase {
					return fmt.Errorf("component %q requests unsupported resource %q", component.Name, resource)
				}
			}
		}
		seenTasks := make(map[int]bool)
		for _, index := range w.BackgroundTasks {
			if index < 0 || index >= len(w.Constructors) || seenTasks[index] {
				return fmt.Errorf("component %q has an invalid or duplicate background task index %d", component.Name, index)
			}
			seenTasks[index] = true
		}
		for _, index := range constructorMetadataIndexes(w.ConstructorReturnsError) {
			if err := check(index, "error-returning"); err != nil {
				return err
			}
		}
		for _, index := range constructorMetadataIndexes(w.ConstructorCollections) {
			if err := check(index, "collection"); err != nil {
				return err
			}
		}
		for _, index := range constructorMetadataIndexes(w.ConstructorDeps) {
			if err := check(index, "dependency"); err != nil {
				return err
			}
		}
		for _, index := range constructorMetadataIndexes(w.ConstructorDeferCalls) {
			if err := check(index, "cleanup"); err != nil {
				return err
			}
		}
		if w.MiddlewareConstructor != nil {
			if err := check(*w.MiddlewareConstructor, "primary middleware"); err != nil {
				return err
			}
			if w.MiddlewareWrapExpr == "" {
				return fmt.Errorf("component %q declares MiddlewareConstructor but no MiddlewareWrapExpr", component.Name)
			}
			if primarySeen {
				return fmt.Errorf("components %q and %q both declare primary middleware", primaryName, component.Name)
			}
			primarySeen, primaryName = true, component.Name
		} else if w.MiddlewareWrapExpr != "" {
			return fmt.Errorf("component %q declares MiddlewareWrapExpr without MiddlewareConstructor", component.Name)
		}
		for i, middleware := range w.Middlewares {
			if err := check(middleware.ConstructorIndex, "inner middleware"); err != nil {
				return err
			}
			if middleware.WrapExpr == "" {
				return fmt.Errorf("component %q declares Middlewares[%d] but no WrapExpr", component.Name, i)
			}
		}
		for i, middleware := range w.OuterMiddlewares {
			if err := check(middleware.ConstructorIndex, "outer middleware"); err != nil {
				return err
			}
			if middleware.WrapExpr == "" {
				return fmt.Errorf("component %q declares OuterMiddlewares[%d] but no WrapExpr", component.Name, i)
			}
		}
	}
	return nil
}

func constructorMetadataIndexes[T any](values map[int]T) []int {
	indexes := make([]int, 0, len(values))
	for index := range values {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}
