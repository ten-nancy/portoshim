package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/ten-nancy/porto/src/api/go/porto"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
)

func getNameValue(input string) string {
	protoKV := strings.Split(input, ",")
	const nameEntrance = "name"
	var nameValue string
	for _, kv := range protoKV {
		kv = strings.TrimSpace(kv)
		kvArray := strings.Split(kv, "=")
		if len(kvArray) != 2 {
			continue
		}

		kvArray[0] = strings.TrimSpace(kvArray[0])
		if kvArray[0] == nameEntrance {
			nameValue = strings.TrimSpace(kvArray[1])
		}
	}
	return nameValue
}

func getProtobufName(ctx context.Context, s any, fieldName string) (string, error) {
	t := reflect.TypeOf(s)

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return "", fmt.Errorf("input must be a struct or a pointer to a struct")
	}

	field, ok := t.FieldByName(fieldName)
	if !ok {
		return "", fmt.Errorf("field %q not found", fieldName)
	}

	protobufTag := field.Tag.Get("protobuf")
	if protobufTag == "" {
		return "", fmt.Errorf("protobuf tag not found on field %q", fieldName)
	}
	nameValue := getNameValue(protobufTag)
	if nameValue == "" {
		return "", fmt.Errorf("'name' not found in protobuf tag of field %q", fieldName)
	}

	return nameValue, nil
}

func setPropertyByName(s any, fieldName string, value any) error {
	v := reflect.ValueOf(s)
	if v.Kind() != reflect.Ptr {
		return fmt.Errorf("input must be a pointer")
	}

	v = v.Elem()

	if v.Kind() != reflect.Struct {
		return fmt.Errorf("input must be a pointer to a struct")
	}

	fieldVal := v.FieldByName(fieldName)
	if !fieldVal.IsValid() {
		return fmt.Errorf("field %q not found", fieldName)
	}

	if !fieldVal.CanSet() {
		return fmt.Errorf("field %q cannot be set", fieldName)
	}

	val := reflect.ValueOf(value)

	if val.Type().ConvertibleTo(fieldVal.Type()) {
		fieldVal.Set(val.Convert(fieldVal.Type()))
	} else {
		return fmt.Errorf("cannot assign value of type %v to field %v of type %v",
			val.Type(), fieldName, fieldVal.Type())
	}

	return nil
}

func isPortoNotSupportedError(err error) bool {
	notSupportedError, ok := err.(*porto.PortoError)
	return ok && notSupportedError.Code == pb.EError_NotSupported
}

// This function safelly sets property of TContainerSpec, frankly saying for any type,
// but it was intended for TContainerSpec
// it checks whether property are supported by portod
// it doesn't cache result, in any case, every CRI request portoshim reconnects to portod.
// it assumes property name could be obtained from struct's tag
func setPropertyByNameInPorto(ctx context.Context, s any, fieldName string, value any) error {
	cl := getPortoClient(ctx)
	propName, err := getProtobufName(ctx, s, fieldName)
	if err != nil {
		return fmt.Errorf("%v can't get property name for %s", err, fieldName)
	}

	_, err = cl.GetProperty("/", propName)
	if err == nil {
		return setPropertyByName(s, fieldName, value)
	} else if isPortoNotSupportedError(err) {
		DebugLog(ctx, "property %s not supported, err: %v", propName, err)
		return nil
	}
	return err
}
