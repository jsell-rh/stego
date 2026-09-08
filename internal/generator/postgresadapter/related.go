package postgresadapter

import (
	"bytes"
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

// emitRelatedFilter emits only relationships declared in the entity schema.
func emitRelatedFilter(buf *bytes.Buffer, entities []types.Entity) {
	fmt.Fprintln(buf, `func (s *Store) applyRelated(ctx context.Context, query *gorm.DB, target string, filters []stegostorage.RelatedFilter) (*gorm.DB, error) {
 if len(filters) > 8 { return nil, fmt.Errorf("too many related filters") }
 for _, filter := range filters {
  if len(filter.Values) > 16 { return nil, fmt.Errorf("too many related filter fields") }
  var related *gorm.DB
  var columns map[string]bool
  switch filter.Entity {`)
	for _, entity := range entities {
		fmt.Fprintf(buf, "case %q:\n", entity.Name)
		fmt.Fprintln(buf, "switch filter.ForeignField {")
		for _, field := range entity.Fields {
			if field.Type != types.FieldTypeRef {
				continue
			}
			fmt.Fprintf(buf, "case %q: if target != %q { return nil, fmt.Errorf(\"related filter target does not match its reference\") }\n", field.Name, field.To)
		}
		fmt.Fprintln(buf, "default: return nil, fmt.Errorf(\"related filter requires a declared reference\")\n}")
		fmt.Fprintf(buf, "related = s.db.WithContext(ctx).Model(&%s{}).Select(filter.ForeignField)\n", entity.Name)
		fmt.Fprint(buf, "columns = map[string]bool{")
		for _, column := range allColumns(entity) {
			fmt.Fprintf(buf, "%q:true,", column)
		}
		fmt.Fprintln(buf, "}")
	}
	fmt.Fprintln(buf, `default: return nil, fmt.Errorf("unknown related entity")
  }
  for field, values := range filter.Values {
   if !columns[field] || len(values) > 100 { return nil, fmt.Errorf("invalid related filter field or value count") }
   for _, value := range values {
    if len(value) > 4096 { return nil, fmt.Errorf("related filter value exceeds size limit") }
   }
   related = related.Where(clause.IN{Column: clause.Column{Name:field}, Values: relatedValues(values)})
  }
  query = query.Where("id IN (?)", related)
 }
 return query, nil
}

func relatedValues(values []string) []any {
 result := make([]any, len(values))
 for i, value := range values { result[i] = value }
 return result
}`)
}
