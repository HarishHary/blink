package configuration

import (
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/harishhary/blink/internal/errors"
)

func LoadFromEnvironment(configuration any) errors.Error {
	value := reflect.ValueOf(configuration).Elem()
	ttype := reflect.TypeOf(configuration).Elem()

	for i := 0; i < ttype.NumField(); i++ {
		fieldType := ttype.Field(i)
		fieldValue := value.Field(i)
		if !fieldType.IsExported() {
			continue
		}

		switch fieldType.Type.Kind() {
		case reflect.Struct:
			if err := LoadFromEnvironment(fieldValue.Addr().Interface()); err != nil {
				return err
			}
		default:
			if err := setField(fieldType, fieldValue); err != nil {
				return err
			}
		}
	}

	return nil
}

func setField(field reflect.StructField, value reflect.Value) errors.Error {
	extract := func(tag string) (string, bool) {
		values := strings.Split(tag, ",")
		return values[0], len(values) > 1 && values[1] == "optional"
	}

	tag := field.Tag.Get("env")
	if tag != "" {
		target, optional := extract(tag)
		return setFieldWithEnvVar(value, target, optional)
	}

	tag = field.Tag.Get("file")
	if tag != "" {
		target, optional := extract(tag)
		return setFieldWithFile(value, target, optional)
	}

	return nil
}

func setFieldWithFile(field reflect.Value, filepath string, optional bool) errors.Error {
	data, err := os.ReadFile(filepath)
	if err != nil && optional {
		return nil
	}

	if err != nil {
		return errors.NewE(err)
	}

	switch field.Kind() {
	case reflect.String:
		field.SetString(string(data))
		return nil
	default:
		return errors.NewF("unsupported type %s", field.Kind().String())
	}
}

func setFieldWithEnvVar(field reflect.Value, env string, optional bool) errors.Error {
	envValue := os.Getenv(env)
	if envValue == "" && !optional {
		return errors.NewF("variable %s is required", env)
	}

	if envValue == "" {
		return nil
	}

	switch kind := field.Kind(); {
	case kind == reflect.Bool:
		v, err := strconv.ParseBool(envValue)
		if err != nil {
			return errors.NewE(err)
		}
		field.SetBool(v)

	case kind == reflect.Int:
		v, err := strconv.Atoi(envValue)
		if err != nil {
			return errors.NewE(err)
		}
		field.SetInt(int64(v))

	case kind == reflect.Uint:
		v, err := strconv.Atoi(envValue)
		if err != nil {
			return errors.NewE(err)
		}
		field.SetUint(uint64(v))

	case kind == reflect.String:
		field.SetString(envValue)

	case field.CanConvert(reflect.TypeOf(&url.URL{})):
		url, err := url.Parse(envValue)
		if err != nil {
			return errors.NewE(err)
		}

		field.Set(reflect.ValueOf(url))

	default:
		return errors.NewF("unsupported type %s", field.Kind().String())
	}

	return nil
}
