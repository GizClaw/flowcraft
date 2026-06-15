package shared

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	oai "github.com/openai/openai-go"
)

func ClassifyAPIError(err error) error {
	return ClassifyAPIErrorWithProvider(DefaultProviderName, err)
}

func ClassifyAPIErrorWithProvider(provider string, err error) error {
	if err == nil {
		return nil
	}
	if errdefs.HasClassification(err) {
		return err
	}
	var ae *oai.Error
	if !errors.As(err, &ae) {
		return errdefs.ClassifyProviderError(provider, err)
	}
	switch ae.StatusCode {
	case 401, 403:
		return errdefs.Unauthorized(err)
	case 402:
		return errdefs.Forbidden(err)
	case 429:
		return errdefs.RateLimit(err)
	case 400, 405, 422:
		return errdefs.Validation(err)
	case 404:
		if ae.Code == "" {
			return errdefs.NotAvailable(err)
		}
		return errdefs.Validation(err)
	case 408, 409:
		return errdefs.NotAvailable(err)
	}
	if ae.StatusCode >= 500 {
		return errdefs.NotAvailable(err)
	}
	return errdefs.ClassifyProviderError(provider, err)
}
