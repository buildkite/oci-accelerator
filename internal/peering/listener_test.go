package peering

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestLimitListenerConnectionCap(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := LimitListener(listener, 1)
	defer limited.Close()
	client1, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client1.Close()
	accepted1, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	client2, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	result := make(chan net.Conn, 1)
	go func() { conn, _ := limited.Accept(); result <- conn }()
	select {
	case <-result:
		t.Fatal("accepted above connection cap")
	case <-time.After(20 * time.Millisecond):
	}
	if err := accepted1.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case accepted2 := <-result:
		if accepted2 == nil {
			t.Fatal("second Accept failed")
		}
		accepted2.Close()
	case <-time.After(time.Second):
		t.Fatal("second Accept remained blocked after slot release")
	}
}

func TestLimitListenerCloseUnblocksAcceptWhenFull(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := LimitListener(listener, 1)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	result := make(chan error, 1)
	go func() { _, err := limited.Accept(); result <- err }()
	if err := limited.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock full Accept")
	}
}
