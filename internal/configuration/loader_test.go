package configuration

import (
	"net/url"
	"os"
	"testing"
)

func TestConfigurationLoaderString(t *testing.T) {
	cases := []struct {
		executor func()
		envvars  map[string]string
	}{
		{
			executor: func() {
				type S struct {
					Foo string `env:"FOO"`
				}

				config := S{}
				expected := S{Foo: "bar"}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %s, got %s", expected, config)
				}
			},
			envvars: map[string]string{
				"FOO": "bar",
			},
		},
		{
			executor: func() {
				type S struct {
					foo string `env:"FOO"`
				}

				config := S{}
				expected := S{foo: ""}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %s, got %s", expected, config)
				}
			},
			envvars: map[string]string{
				"FOO": "bar",
			},
		},
		{
			executor: func() {
				type S struct {
					Foo int `env:"FOO"`
				}

				config := S{}
				expected := S{Foo: 42}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %v, got %v", expected, config)
				}
			},
			envvars: map[string]string{
				"FOO": "42",
			},
		},
		{
			executor: func() {
				type S struct {
					Foo uint `env:"FOO"`
				}

				config := S{}
				expected := S{Foo: 42}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %v, got %v", expected, config)
				}
			},
			envvars: map[string]string{
				"FOO": "42",
			},
		},
		{
			executor: func() {
				type S struct {
					Foo bool `env:"FOO"`
				}

				config := S{}
				expected := S{Foo: true}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %v, got %v", expected, config)
				}
			},
			envvars: map[string]string{
				"FOO": "true",
			},
		},
		{
			executor: func() {
				type S struct {
					Foo1 bool   `env:"FOO1"`
					Foo2 int    `env:"FOO2"`
					Foo3 string `env:"FOO3"`
				}

				config := S{}
				expected := S{Foo1: true,
					Foo2: 42,
					Foo3: "bar"}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %v, got %v", expected, config)
				}
			},
			envvars: map[string]string{
				"FOO1": "true",
				"FOO2": "42",
				"FOO3": "bar",
			},
		},
		{
			executor: func() {
				type S struct {
					Outer string `env:"OUTER"`
					Inner struct {
						Inner1 int    `env:"INNER1"`
						Inner2 string `env:"INNER2"`
					}
				}

				config := S{}
				expected := S{
					Outer: "outer",
					Inner: struct {
						Inner1 int    "env:\"INNER1\""
						Inner2 string "env:\"INNER2\""
					}{
						Inner1: 42,
						Inner2: "bar",
					},
				}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config != expected {
					t.Errorf("expected %v, got %v", expected, config)
				}
			},
			envvars: map[string]string{
				"OUTER":  "outer",
				"INNER1": "42",
				"INNER2": "bar",
			},
		},
		{
			executor: func() {
				type S struct {
					URL *url.URL `env:"URL"`
				}

				config := S{}
				url, _ := url.Parse("http://localhost")
				expected := S{
					URL: url,
				}
				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config.URL.String() != expected.URL.String() {
					t.Errorf("expected %v, got %v", expected, config)
				}
			},
			envvars: map[string]string{
				"URL": "http://localhost",
			},
		},
		{
			executor: func() {
				type S struct {
					URL *url.URL `env:"URL"`
				}

				config := S{}
				err := LoadFromEnvironment(&config)
				if err == nil {
					t.Error("expected an error but got none")
				}
			},
			envvars: map[string]string{
				"URL": ":localhost",
			},
		},
		{
			executor: func() {
				type S struct {
					URL *url.URL `env:"URL"`
				}

				config := S{}
				err := LoadFromEnvironment(&config)
				if err == nil {
					t.Error("expected an error but got none")
				}
			},
			envvars: map[string]string{
				"URL": ":localhost",
			},
		},
		{
			executor: func() {
				type S struct {
					F string `file:"my_file"`
				}

				config := S{}
				file, _ := os.Create("my_file")
				defer os.Remove("my_file")
				file.WriteString("test")

				err := LoadFromEnvironment(&config)
				if err != nil {
					t.Error(err)
				}

				if config.F != "test" {
					t.Errorf("expected '{ F: test }' but got %v", config)
				}
			},
			envvars: map[string]string{
				"my_file": "test",
			},
		},
	}

	for _, ccase := range cases {
		os.Clearenv()
		for name, value := range ccase.envvars {
			os.Setenv(name, value)
		}

		ccase.executor()
	}

}
