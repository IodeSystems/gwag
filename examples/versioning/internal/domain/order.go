// Package domain is the orders model the handlers of every API version
// work on. It belongs to no version: v2 maps its wire types onto it,
// and so does v1. A v3 would map onto it too, never onto v2.
package domain

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Status of an order.
type Status string

const (
	StatusOpen     Status = "open"
	StatusClosed   Status = "closed"
	StatusRefunded Status = "refunded"
)

// Order is one order. Money is integer minor units (cents) in
// Currency, an ISO 4217 code.
type Order struct {
	ID            string
	AmountCents   int64
	Currency      string
	Customer      string
	Status        Status
	RefundedCents int64
}

// ErrNotFound is returned for an unknown order id.
var ErrNotFound = errors.New("order not found")

// ErrInvalid wraps a refused change.
var ErrInvalid = errors.New("invalid")

// Store holds orders in memory.
type Store struct {
	mu     sync.Mutex
	orders map[string]Order
}

// NewStore returns a store with the example's seed orders.
func NewStore() *Store {
	s := &Store{orders: map[string]Order{}}
	for _, o := range Seed() {
		s.orders[o.ID] = o
	}
	return s
}

// Seed is the data both the store and the legacy service start with.
// o-3 is in EUR, which the legacy service could not hold.
func Seed() []Order {
	return []Order{
		{ID: "o-1", AmountCents: 1250, Currency: "USD", Customer: "Ada Lovelace", Status: StatusOpen},
		{ID: "o-2", AmountCents: 999, Currency: "USD", Customer: "Grace Hopper", Status: StatusClosed},
		{ID: "o-3", AmountCents: 4200, Currency: "EUR", Customer: "Edsger Dijkstra", Status: StatusOpen},
	}
}

// Get returns the order with id.
func (s *Store) Get(id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	return o, nil
}

// Filter selects orders in List. Zero values match everything.
type Filter struct {
	MinAmountCents int64
	Currency       string
}

// List returns the orders that match f, by id.
func (s *Store) List(f Filter) []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Order{}
	for _, o := range s.orders {
		if o.AmountCents < f.MinAmountCents {
			continue
		}
		if f.Currency != "" && o.Currency != f.Currency {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Refund returns cents to the customer. A refund of the whole
// remaining amount marks the order refunded.
func (s *Store) Refund(id string, cents int64) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	left := o.AmountCents - o.RefundedCents
	if cents <= 0 || cents > left {
		return Order{}, fmt.Errorf("%w: refund %d cents, %d left to refund", ErrInvalid, cents, left)
	}
	o.RefundedCents += cents
	if o.RefundedCents == o.AmountCents {
		o.Status = StatusRefunded
	}
	s.orders[id] = o
	return o, nil
}

// SetAmount replaces the amount of an open order with no refunds. Only
// v1's applyDiscount uses it.
func (s *Store) SetAmount(id string, cents int64) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	if o.Status != StatusOpen || o.RefundedCents != 0 {
		return Order{}, fmt.Errorf("%w: order %s is %s", ErrInvalid, id, o.Status)
	}
	o.AmountCents = cents
	s.orders[id] = o
	return o, nil
}
