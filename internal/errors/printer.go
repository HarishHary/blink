package errors

import (
	"fmt"
	"reflect"
	"strings"
)

func Print(obj any) string {
	value := reflect.ValueOf(obj)

	writer := strings.Builder{}
	PrintValue(&writer, &value)
	return writer.String()
}

func PrintValue(writer *strings.Builder, value *reflect.Value) {
	ttype := value.Type()

	switch ttype.Kind() {
	case reflect.Struct:
		PrintStruct(writer, value)
	case reflect.Ptr:
		value := value.Elem()
		PrintStruct(writer, &value)
	case reflect.Func:
		return
	default:
		writer.WriteString(fmt.Sprintf("%s", value.Interface()))
	}
}

func PrintStruct(writer *strings.Builder, value *reflect.Value) {
	ttype := value.Type()

	writer.WriteString(fmt.Sprintf("%s({", ttype.Name()))
	for i := 0; i < ttype.NumField(); i++ {
		field := ttype.Field(i)
		fieldValue := value.Field(i)
		printField(writer, &field, &fieldValue)
	}

	writer.WriteString(" })")
}

func printField(writer *strings.Builder, field *reflect.StructField, value *reflect.Value) {
	if !value.CanInterface() {
		return
	}

	tag, _ := field.Tag.Lookup("obfuscate")
	writer.WriteString(" ")
	switch {
	case tag == "true":
		writer.WriteString(fmt.Sprintf("%s(%s)", field.Name, "******"))

	case field.Type.Kind() == reflect.Struct:
		PrintStruct(writer, value)

	case field.Type.Kind() == reflect.Func:
		return

	default:
		writer.WriteString(fmt.Sprintf("%s(%s)", field.Name, value.Interface()))
	}
}
