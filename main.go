// package main

// import (
// 	"context"
// 	"log"
// 	"os"
// 	"path/filepath"

// 	"github.com/harishhary/blink/src/shared/formatters"
// )

// 	// Example data to format
// 	data := map[string]any{"message": "this is a test"}

// 	for name, formatter := range formatterRepository.Formatters {
// 		success, err := formatter.Format(context.Background(), data)
// 		if err != nil {
// 			log.Printf("Failed to format using %s: %v", name, err)
// 		} else if success {
// 			log.Printf("Formatted data using %s: %v", name, data)
// 			log.Printf("Formatter '%s'", formatter.String())
// 		}
// 	}
// 	// Continue with processing the formatted data
// }

// func LoadPlugins[T any](paths []string) ([]T, error) {
// 	var plugins []T
// 	for _, path := range paths {
// 		p, err := plugin.Open(path)
// 		if err != nil {
// 			return nil, err
// 		}
// 		sym, err := p.Lookup("Plugin")
// 		if err != nil {
// 			return nil, err
// 		}
// 		pluginInstance, ok := sym.(T)
// 		if !ok {
// 			return nil, fmt.Errorf("invalid type for plugin %s", path)
// 		}
// 		plugins = append(plugins, pluginInstance)
// 	}
// 	return plugins, nil
// }

package main

import (
	"log"

	"github.com/harishhary/blink/pkg/rules"
)

// var formatterRepository = formatters.GetFormatterRepository()
var repo = rules.GetRuleRepository()

func main() {

	// pluginDir := "./examples/rules"
	// err := filepath.Walk(pluginDir, func(path string, info os.FileInfo, err error) error {
	// 	if err != nil {
	// 		return err
	// 	}
	// 	log.Printf("Plugin path: %s", path)
	// 	if !info.IsDir() && filepath.Ext(path) == ".so" {
	// 		if err := repo.Load(path); err != nil {
	// 			log.Printf("Failed to load plugin %s: %v", path, err)
	// 		}
	// 	}
	// 	return nil
	// })
	// if err != nil {
	// 	log.Fatalf("Failed to load plugins: %v", err)
	// }
	// for rule := range repo.Data {
	// 	log.Printf("Rule: %s", rule)
	// }
	// data := map[string]any{"message": "this is a test"}

	// for name, formatter := range formatterRepository.Formatters {
	// 	success, err := formatter.Format(context.Background(), data)
	// 	if err != nil {
	// 		log.Printf("Failed to format using %s: %v", name, err)
	// 	} else if success {
	// 		log.Printf("Formatted data using %s: %v", name, data)
	// 		log.Printf("Formatter '%s'", formatter.String())
	// 	}
	// }
	// Continue with processing the formatted data

	repo := rules.GetRuleRepository()
	if err := repo.Load("/Users/harish.segar/Documents/Research/blink/examples/rules/"); err != nil {
		log.Fatalf("Error loading plugin: %v", err)
	}

}
