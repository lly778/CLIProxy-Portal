package cpamp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ListAuthFileModels reads models actually registered to one credential.
// It never downloads credentials or guesses models from the static catalog.
func (c *Client) ListAuthFileModels(ctx context.Context, file AuthFile) ([]Model, error) {
	name := strings.TrimSpace(file.RuntimeID)
	if name == "" {
		name = strings.TrimSpace(file.Name)
	}
	if name == "" || strings.TrimSpace(file.AuthIndex) == "" {
		return nil, errors.New("cpamp auth file identity is required for registered models")
	}
	query := url.Values{"name": {name}, "auth_index": {strings.TrimSpace(file.AuthIndex)}}
	path := pathAuthFiles + "/models?" + query.Encode()
	body, err := c.do(ctx, http.MethodGet, path, nil, c.adminHeader)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Models []Model `json:"models"`
	}
	if err := decodeJSON(pathAuthFiles+"/models", body, &envelope); err != nil {
		return nil, err
	}
	return envelope.Models, nil
}
