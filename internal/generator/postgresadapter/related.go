package postgresadapter

import (
	"bytes"
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// emitRelatedFilter restricts joins to keys that identify the same entity.
func emitRelatedFilter(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintln(buf, `func (s *Store) applyRelated(ctx context.Context, query *gorm.DB, target string, filters []stegostorage.RelatedFilter) (*gorm.DB, error) {
 if len(filters) > 8 { return nil, fmt.Errorf("too many related filters") }
 for _, filter := range filters {
  expression, err := s.relatedExpression(ctx, target, filter)
  if err != nil { return nil, err }
  query = query.Where(expression)
 }
 return query, nil
}

func referenceTarget(entity, field string) string {
 switch entity {`)
	for _, entity := range entities {
		fmt.Fprintf(buf, "case %q: switch field {\ncase \"id\": return %q\n", entity.Name, entity.Name)
		for _, field := range entity.Fields {
			if field.Type == types.FieldTypeRef {
				fmt.Fprintf(buf, "case %q: return %q\n", field.Name, field.To)
			}
		}
		fmt.Fprintln(buf, "}")
	}
	fmt.Fprintln(buf, `}
 return ""
}

func filterColumns(entity string) map[string]bool {
 switch entity {`)
	for _, entity := range entities {
		fmt.Fprintf(buf, "case %q: return map[string]bool{", entity.Name)
		for _, column := range allColumns(entity) {
			fmt.Fprintf(buf, "%q:true,", column)
		}
		fmt.Fprintln(buf, "}")
	}
	fmt.Fprintln(buf, `}
 return nil
}

func (s *Store) relatedExpression(ctx context.Context, target string, filter stegostorage.RelatedFilter) (clause.Expression, error) {
 if len(filter.Values) > 16 { return nil, fmt.Errorf("too many related filter fields") }
 local := filter.LocalField
 if local == "" { local = "id" }
 identity := referenceTarget(target, local)
 if identity == "" || identity != referenceTarget(filter.Entity, filter.ForeignField) {
  return nil, fmt.Errorf("related filter requires keys for the same declared entity")
 }
 var related *gorm.DB
 switch filter.Entity {`)
	for _, entity := range entities {
		fmt.Fprintf(buf, "case %q: related = s.db.WithContext(ctx).Model(&%s{}).Select(filter.ForeignField)\n", entity.Name, entity.Name)
	}
	fmt.Fprintln(buf, `default: return nil, fmt.Errorf("unknown related entity")
 }
 columns := filterColumns(filter.Entity)
 for field, values := range filter.Values {
  if !columns[field] { return nil, fmt.Errorf("invalid related filter field") }
  if err := checkFilterValues(values); err != nil { return nil, err }
  related = related.Where(clause.IN{Column: clause.Column{Name:field}, Values: relatedValues(values)})
 }
 return clause.Expr{SQL: "? IN (?)", Vars: []any{clause.Column{Name:local}, related}}, nil
}

func checkFilterValues(values []string) error {
 if len(values) > 100 { return fmt.Errorf("too many filter values") }
 for _, value := range values {
  if len(value) > 4096 { return fmt.Errorf("filter value exceeds size limit") }
 }
 return nil
}

func relatedValues(values []string) []any {
 result := make([]any, len(values))
 for i, value := range values { result[i] = value }
 return result
}

func (s *Store) applyRowFilter(ctx context.Context, query *gorm.DB, entity string, filter *stegostorage.RowFilter) (*gorm.DB, error) {
 if filter == nil { return query, nil }
 nodes, bytes := 0, 0
 expression, err := s.rowExpression(ctx, entity, *filter, 1, &nodes, &bytes)
 if err != nil { return nil, err }
 return query.Where(expression), nil
}

func (s *Store) rowExpression(ctx context.Context, entity string, filter stegostorage.RowFilter, depth int, nodes, bytes *int) (clause.Expression, error) {
 *nodes++
 if depth > 8 || *nodes > 64 { return nil, fmt.Errorf("row filter exceeds structure limit") }
 modes := 0
 if filter.Field != "" { modes++ }
 if filter.Related != nil { modes++ }
 if filter.All != nil { modes++ }
 if filter.Any != nil { modes++ }
 if modes != 1 || (filter.Field == "" && filter.Values != nil) { return nil, fmt.Errorf("row filter requires one condition") }
 values := filter.Values
 if filter.Related != nil {
  for _, group := range filter.Related.Values {
   for _, value := range group { *bytes += len(value) }
  }
 }
 for _, value := range values { *bytes += len(value) }
 if *bytes > 65536 { return nil, fmt.Errorf("row filter exceeds value size limit") }
 if filter.Field != "" {
  if !filterColumns(entity)[filter.Field] { return nil, fmt.Errorf("invalid row filter field") }
  if err := checkFilterValues(values); err != nil { return nil, err }
  return clause.IN{Column:clause.Column{Name:filter.Field}, Values:relatedValues(values)}, nil
 }
 if filter.Related != nil { return s.relatedExpression(ctx, entity, *filter.Related) }
 children := filter.All
 if filter.Any != nil { children = filter.Any }
 if len(children) == 0 || len(children) > 64 { return nil, fmt.Errorf("invalid row filter group size") }
 expressions := make([]clause.Expression, 0, len(children))
 for _, child := range children {
  expression, err := s.rowExpression(ctx, entity, child, depth+1, nodes, bytes)
  if err != nil { return nil, err }
  expressions = append(expressions, expression)
 }
 if filter.Any != nil { return clause.Or(expressions...), nil }
 return clause.And(expressions...), nil
}`)
}
