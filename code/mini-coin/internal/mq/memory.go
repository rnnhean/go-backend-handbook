package mq

import (
	"context"
	"sync"
)

type Message struct {
	Topic   string
	Payload any
}

type MemoryProducer struct {
	mu       sync.Mutex
	messages []Message
}

func NewMemoryProducer() *MemoryProducer { return &MemoryProducer{} }

func (p *MemoryProducer) Publish(_ context.Context, topic string, payload any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, Message{Topic: topic, Payload: payload})
	return nil
}

func (p *MemoryProducer) Messages() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Message(nil), p.messages...)
}
