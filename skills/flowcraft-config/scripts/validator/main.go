package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/utils"
)

const (
	exitOK    = 0
	exitError = 1
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	typeName := fs.String("type", "deploy", "deploy|inference|workspace|sandbox|tool|graph|agent")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: validate-config [--type TYPE] <file>")
		return exitError
	}

	data, err := readInput(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] read input: %v\n", err)
		return exitError
	}

	switch *typeName {
	case "deploy":
		if err := validateDeploy(data); err != nil {
			fmt.Fprintf(os.Stderr, "[build] %v\n", err)
			return exitError
		}
	case "graph":
		if err := validateGraph(data); err != nil {
			fmt.Fprintf(os.Stderr, "[graph] %v\n", err)
			return exitError
		}
	case "inference", "workspace", "sandbox", "tool", "agent":
		if err := validateGeneric(*typeName, data); err != nil {
			fmt.Fprintf(os.Stderr, "[parse] %v\n", err)
			return exitError
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown --type %q\n", *typeName)
		return exitError
	}
	return exitOK
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func validateDeploy(data []byte) error {
	doc, err := deploy.Parse(data)
	if err != nil {
		return err
	}
	if doc.Runtime != nil {
		if _, err := runtime.DecodeConfig(doc); err != nil {
			return err
		}
	}
	fmt.Printf("OK: deployment document (%d resources, %d agents)\n",
		len(doc.Resources), len(doc.Agents))
	return nil
}

func validateGraph(data []byte) error {
	jsonData, err := utils.ToJSON(data)
	if err != nil {
		return err
	}
	var definition graph.GraphDefinition
	if err := json.Unmarshal(jsonData, &definition); err != nil {
		return err
	}
	if err := definition.Validate(); err != nil {
		return err
	}
	fmt.Printf("OK: graph definition (%d nodes, %d edges)\n",
		len(definition.Nodes), len(definition.Edges))
	return nil
}

func validateGeneric(kind string, data []byte) error {
	jsonData, err := utils.ToJSON(data)
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(jsonData, &value); err != nil {
		return err
	}
	fmt.Printf("OK: %s sub-document\n", kind)
	return nil
}
