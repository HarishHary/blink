package errors

import (
	"log"
	"strings"
	"testing"
)

func TestSimpleObfuscation(t *testing.T) {
	type ObfuscatedType struct {
		Username string
		Password string `obfuscate:"true"`
	}

	value := ObfuscatedType{Username: "test", Password: "helloworld"}
	serialized := Print(value)
	if strings.Contains(serialized, "helloworld") {
		t.Errorf("Password has not been obfuscated, was %s", serialized)
	}
}

func TestComplexObfuscation(t *testing.T) {
	type ComplexPassword struct {
		Type  string
		Value string `obfuscate:"true"`
	}

	type ObfuscatedType struct {
		Username string
		Password ComplexPassword
	}

	value := ObfuscatedType{Username: "test", Password: ComplexPassword{Type: "standard", Value: "helloworld"}}
	serialized := Print(value)
	if strings.Contains(serialized, "helloworld") {
		t.Errorf("Password has not been obfuscated, was %s", serialized)
	}

	log.Println(serialized)
}

func TestTopLevelObfuscation(t *testing.T) {
	type ComplexPassword struct {
		Type  string
		Value string
	}

	type ObfuscatedType struct {
		Username string
		Password ComplexPassword `obfuscate:"true"`
	}

	value := ObfuscatedType{Username: "test", Password: ComplexPassword{Type: "standard", Value: "helloworld"}}
	serialized := Print(value)
	if strings.Contains(serialized, "helloworld") {
		t.Errorf("Password has not been obfuscated, was %s", serialized)
	}

	log.Println(serialized)
}
