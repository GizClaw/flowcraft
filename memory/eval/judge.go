package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// judgeResponse asks the judge model whether the prediction answers the gold.
func judgeResponse(
	ctx context.Context,
	runtime *inference.Runtime,
	model inference.ModelRef,
	golds []string,
	prediction string,
) (float64, error) {
	input, err := buildJudgeInput(golds, prediction)
	if err != nil {
		return 0, err
	}
	response, err := generateWithSystem(ctx, runtime, model, judgeSystem, input)
	if err != nil {
		return 0, err
	}
	lower := strings.ToLower(strings.TrimSpace(response))
	if strings.Contains(lower, "incorrect") {
		return 0, nil
	}
	if strings.Contains(lower, "correct") {
		return 1, nil
	}
	return 0, fmt.Errorf("judge response %q is not CORRECT or INCORRECT", response)
}
