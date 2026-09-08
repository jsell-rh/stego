package gen

// UnicodeEscapeValidation is shared by generated JSON input boundaries.
// The caller must import strconv and also check JSON syntax and UTF-8.
const UnicodeEscapeValidation = `
// encoding/json replaces unmatched UTF-16 surrogates. Reject these sequences
// before decoding so distinct invalid inputs cannot become one stored value.
func validUnicodeEscapes(data []byte)bool{
 for i:=0;i<len(data);i++{
  if data[i]!='\\'{continue}
  i++
  if i>=len(data){return false}
  if data[i]!='u'{continue}
  if i+4>=len(data){return false}
  value,err:=strconv.ParseUint(string(data[i+1:i+5]),16,16)
  if err!=nil{return false}
  i+=4
  if value>=0xDC00&&value<=0xDFFF{return false}
  if value<0xD800||value>0xDBFF{continue}
  if i+6>=len(data)||data[i+1]!='\\'||data[i+2]!='u'{return false}
  low,err:=strconv.ParseUint(string(data[i+3:i+7]),16,16)
  if err!=nil||low<0xDC00||low>0xDFFF{return false}
  i+=6
 }
 return true
}
`
