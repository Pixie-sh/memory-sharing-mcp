// Package provider defines the contract between the gateway and a memory
// provider: the store-specific process or library that runs memory operations.
// The gateway owns validation, authorization, queueing, and restarts; a provider
// only maps each operation to its store. Mnemosyne is in package mnemosyne.
package provider

import (
	"context"
	"encoding/json"
	"errors"
)

// Operations every provider receives, with the arguments the gateway prepares:
//
//	recall   {query string, top_k int}                 -> {results: [Memory]}, best match first
//	remember {content, source string, importance float, author_id string} -> {id}
//	stats    {}                                        -> provider-defined object
//	forget   {id string}                               -> {deleted, hidden bool}; hidden: expired, not deleted
//	sleep    {}                                        -> provider-defined object
//
// IDs are the provider's own; the gateway adds and strips the "<provider>:"
// prefix clients see. A provider that does not support an operation returns
// an *OpError.
type Provider interface {
	// Call runs one operation. An *OpError leaves the provider usable; any other
	// error means the provider is gone and must be replaced, and the call may or
	// may not have taken effect.
	Call(ctx context.Context, op string, args any) (json.RawMessage, error)
	// Close releases the provider, giving up when ctx ends.
	Close(ctx context.Context)
}

// Memory is one recall result. Field names follow mem0's memory items; a
// provider fills what it has and leaves the rest empty, and puts store-specific
// fields (such as source and importance) in Metadata. AgentID is the API key
// that wrote the memory, where the store records it. The gateway sets Provider
// and prefixes ID.
type Memory struct {
	ID        string         `json:"id"`
	Memory    string         `json:"memory"`
	Provider  string         `json:"provider,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Starter starts a provider. The gateway calls it once at startup and again
// after each failure.
type Starter func(ctx context.Context) (Provider, error)

var (
	// ErrUnavailable means the provider is gone; start a new one.
	ErrUnavailable = errors.New("provider unavailable")
	// ErrIndeterminate means the operation may or may not have taken effect.
	ErrIndeterminate = errors.New("provider operation outcome unknown")
)

// OpError is an error the store reported for one operation; the provider stays usable.
// Msg may quote memory content, so it must not be logged or returned to clients.
type OpError struct{ Msg string }

func (e *OpError) Error() string { return e.Msg }
