package runtime

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/goccy/go-json"
)

// DriverFunc builds a provider from a driver's settings block.
//
// The runtime never imports a driver, because importing one drags its vendor SDK
// into every binary that links this package. The host names the drivers it
// compiles in, which also makes "which drivers exist" a fact of the binary
// rather than a side effect of an init function.
type DriverFunc func(settings json.RawMessage) (inference.Provider, error)

// Driver resolves a driver name to a constructor.
type Driver struct {
	// Name is the value written in inference.driver.
	Name string
	// Build constructs the provider; a non-nil error fails the runtime.
	Build DriverFunc
}

func driverTable(drivers []Driver) (map[string]DriverFunc, error) {
	table := make(map[string]DriverFunc, len(drivers))
	for _, driver := range drivers {
		if driver.Name == "" {
			return nil, errorf("runtime: a driver has no name")
		}
		if driver.Build == nil {
			return nil, errorf("runtime: driver %q has no constructor", driver.Name)
		}
		if _, exists := table[driver.Name]; exists {
			return nil, errorf("runtime: driver %q is registered twice", driver.Name)
		}
		table[driver.Name] = driver.Build
	}
	return table, nil
}
