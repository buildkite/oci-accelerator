package peering

import (
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func TestLimitListenerConnectionCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		queued := &queuedAcceptListener{Listener: listener, connections: make(chan net.Conn, 2)}
		limited := LimitListener(queued, 1)
		defer limited.Close()
		for range 2 {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			queued.connections <- server
		}
		accepted1, err := limited.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer accepted1.Close()

		var accepted2 net.Conn
		var acceptErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			accepted2, acceptErr = limited.Accept()
		}()
		defer func() {
			limited.Close()
			<-done
			if accepted2 != nil {
				accepted2.Close()
			}
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("accepted above connection cap")
		default:
		}

		if err := accepted1.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-done:
			if acceptErr != nil || accepted2 == nil {
				t.Fatalf("second Accept = %v, %v", accepted2, acceptErr)
			}
		default:
			t.Fatal("second Accept remained blocked after slot release")
		}
	})
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

type queuedAcceptListener struct {
	net.Listener
	connections chan net.Conn
}

func (l *queuedAcceptListener) Accept() (net.Conn, error) {
	return <-l.connections, nil
}
