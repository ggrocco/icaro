package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"icaro/internal/store"
)

// ConnectionType declares which fields a connection type carries.
type ConnectionType struct {
	Name     string
	Required []string
	// Open allows arbitrary extra fields.
	Open bool
}

// ConnectionTypes is the registry of known types.
var ConnectionTypes = map[string]ConnectionType{
	"generic": {Name: "generic", Open: true},
	"bearer":  {Name: "bearer", Required: []string{"token"}, Open: true},
	"basic":   {Name: "basic", Required: []string{"username", "password"}, Open: true},
}

// ConnectionInfo is the API view: never includes values.
type ConnectionInfo struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	FieldNames []string `json:"field_names"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var fieldRe = regexp.MustCompile(`^[a-zA-Z0-9_]{1,64}$`)

// UpsertConnection encrypts and stores a connection.
func (s *Service) UpsertConnection(ctx context.Context, name, typ string, fields map[string]string) (*ConnectionInfo, error) {
	if s.key == nil {
		return nil, errors.New("master key not configured")
	}
	if !nameRe.MatchString(name) {
		return nil, badRequest("invalid connection name %q", name)
	}
	ct, ok := ConnectionTypes[typ]
	if !ok {
		return nil, badRequest("unknown connection type %q", typ)
	}
	for _, req := range ct.Required {
		if fields[req] == "" {
			return nil, badRequest("connection type %s requires field %q", typ, req)
		}
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		if !fieldRe.MatchString(k) {
			return nil, badRequest("invalid field name %q", k)
		}
		names = append(names, k)
	}
	sort.Strings(names)
	plain, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	ct2, err := s.key.Seal(plain)
	if err != nil {
		return nil, err
	}
	c := &store.Connection{Name: name, Type: typ, FieldsCT: ct2, FieldNames: names}
	if err := s.store.UpsertConnection(ctx, c); err != nil {
		return nil, err
	}
	s.log.Info("connection stored", "name", name, "type", typ)
	stored, err := s.store.GetConnection(ctx, name)
	if err != nil {
		return nil, err
	}
	return connInfo(stored), nil
}

// ListConnections returns names and types only.
func (s *Service) ListConnections(ctx context.Context) ([]*ConnectionInfo, error) {
	cs, err := s.store.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*ConnectionInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, connInfo(c))
	}
	return out, nil
}

// DeleteConnection removes a connection.
func (s *Service) DeleteConnection(ctx context.Context, name string) error {
	err := s.store.DeleteConnection(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// ResolveConnection decrypts a connection's fields (runner use only).
func (s *Service) ResolveConnection(ctx context.Context, name string) (map[string]string, error) {
	if s.key == nil {
		return nil, errors.New("master key not configured")
	}
	c, err := s.store.GetConnection(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("connection %q not found", name)
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.key.Open(c.FieldsCT)
	if err != nil {
		return nil, fmt.Errorf("decrypt connection %q: %w", name, err)
	}
	var fields map[string]string
	if err := json.Unmarshal(plain, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func connInfo(c *store.Connection) *ConnectionInfo {
	return &ConnectionInfo{
		Name: c.Name, Type: c.Type, FieldNames: c.FieldNames,
		CreatedAt: c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), UpdatedAt: c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
