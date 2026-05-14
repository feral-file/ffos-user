package main

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

func TestInitCDPBestEffortDoesNotTreatTemporaryCDPFailureAsFatal(t *testing.T) {
	client := &fakeCDPClient{initErr: errors.New("connection refused")}

	if initCDPBestEffort(context.Background(), client, zap.NewNop()) {
		t.Fatal("expected CDP init to be reported as unavailable")
	}

	if client.initCalls != 1 {
		t.Fatalf("expected one init attempt, got %d", client.initCalls)
	}
}

func TestInitCDPBestEffortReportsSuccessfulInit(t *testing.T) {
	client := &fakeCDPClient{}

	if !initCDPBestEffort(context.Background(), client, zap.NewNop()) {
		t.Fatal("expected CDP init to be reported as available")
	}

	if client.initCalls != 1 {
		t.Fatalf("expected one init attempt, got %d", client.initCalls)
	}
}

type fakeCDPClient struct {
	initErr   error
	initCalls int
}

func (f *fakeCDPClient) Init(context.Context) error {
	f.initCalls++
	return f.initErr
}

func (f *fakeCDPClient) Send(string, map[string]interface{}) (interface{}, error) {
	return nil, nil
}

func (f *fakeCDPClient) Close() {}
