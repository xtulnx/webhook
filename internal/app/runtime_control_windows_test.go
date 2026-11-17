package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRuntimePipeBuffersRequestAndResponse(t *testing.T) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(random[:])
	listener, err := listenRuntimeControl("", id)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	type accepted struct {
		conn net.Conn
		err  error
	}
	ready := make(chan accepted)
	go func() {
		conn, err := listener.Accept()
		select {
		case ready <- accepted{conn, err}:
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	client, err := dialRuntimeControl(ctx, "", id)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var server net.Conn
	select {
	case result := <-ready:
		if result.err != nil {
			t.Fatal(result.err)
		}
		server = result.conn
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer server.Close()
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	request := controlRequest{ID: id, Operation: "identity"}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatalf("write request before server reads: %v", err)
	}
	var received controlRequest
	if err := json.NewDecoder(server).Decode(&received); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if received.ID != id || received.Operation != request.Operation {
		t.Fatalf("unexpected request: %+v", received)
	}
	if err := json.NewEncoder(server).Encode(controlResponse{Instance: runtimeRecord{ID: id}}); err != nil {
		t.Fatalf("write reply before client reads: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	var response controlResponse
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatalf("read reply after server closes: %v", err)
	}
	if response.Instance.ID != id {
		t.Fatalf("unexpected reply: %+v", response)
	}
}

func TestRuntimeDirectoryDACL(t *testing.T) {
	const current = "S-1-5-21-100-200-300-1001"
	const other = "S-1-5-21-100-200-300-1002"
	user, err := windows.StringToSid(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sddl string
		ok   bool
	}{
		{"user-owned", "O:" + current + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + current + ")", true},
		{"admin-owned", "O:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + current + ")", true},
		{"other-account", "O:BAD:P(A;;GA;;;BA)(A;;GA;;;" + other + ")", false},
		{"current-user-on-other-directory", "O:" + other + "D:P(A;;GA;;;" + current + ")", false},
		{"everyone", "O:BAD:P(A;;GA;;;BA)(A;;GA;;;WD)", false},
		{"unprotected", "O:BAD:(A;;GA;;;BA)", false},
		{"empty", "O:BAD:P", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRuntimeDirectoryDACL(sd, user); (err == nil) != tc.ok {
				t.Fatalf("valid = %t, want %t, error = %v", err == nil, tc.ok, err)
			}
		})
	}
}
