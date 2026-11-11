package rpc

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeCommon/pkg/rpcauth"
	"github.com/TeaOSLab/EdgeNode/internal/configs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
)

func TestSecurityRPCPlaintextRequiresExplicitEndpointScope(t *testing.T) {
	t.Setenv("EDGE_ALLOW_PLAINTEXT_RPC", "")
	t.Setenv("EDGE_ALLOW_LOOPBACK_RPC", "")
	newClient := func(endpoint string) (*RPCClient, error) {
		return NewRPCClient(&configs.APIConfig{RPCEndpoints: []string{endpoint}})
	}
	if client, err := newClient("http://127.0.0.1:1"); err == nil {
		client.Close()
		t.Fatal("default configuration accepted plaintext RPC")
	}
	t.Setenv("EDGE_ALLOW_LOOPBACK_RPC", "true")
	for _, endpoint := range []string{"http://192.0.2.1:1", "http://localhost:1", "http://example.test:1"} {
		if client, err := newClient(endpoint); err == nil {
			client.Close()
			t.Fatalf("loopback-only permission accepted %s", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:1", "http://[::1]:1"} {
		client, err := newClient(endpoint)
		if err != nil {
			t.Fatalf("explicit loopback endpoint rejected: %v", err)
		}
		client.Close()
	}
}

type parallelPingServer struct {
	pb.UnimplementedPingServiceServer
}

func (*parallelPingServer) Ping(context.Context, *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{}, nil
}

func TestSecurityRPCParallelEndpointChecksAndConfigurationRollback(t *testing.T) {
	t.Setenv("EDGE_ALLOW_PLAINTEXT_RPC", "")
	t.Setenv("EDGE_ALLOW_LOOPBACK_RPC", "true")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterPingServiceServer(server, &parallelPingServer{})
	go server.Serve(listener)
	defer server.Stop()
	endpoint := "http://" + listener.Addr().String()
	config := &configs.APIConfig{NodeId: "original-node", Secret: "0123456789abcdef0123456789abcdef", RPCEndpoints: []string{endpoint}}
	client, err := NewRPCClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	config.NodeId = "caller-mutated"
	config.RPCEndpoints[0] = "http://untrusted.test:1234"
	if err := client.UpdateConfig(&configs.APIConfig{NodeId: "bad", Secret: "bad", RPCEndpoints: []string{endpoint, "invalid://"}}); err == nil {
		t.Fatal("invalid configuration replaced a working RPC identity")
	}
	md, _ := metadata.FromOutgoingContext(client.Context())
	claims, err := rpcauth.Verify(md.Get("token")[0], "0123456789abcdef0123456789abcdef", "original-node")
	if err != nil || claims.Type != "node" {
		t.Fatalf("RPC identity did not survive caller mutation or failed update: %v", err)
	}
	var endpoints []string
	for i := 0; i < 16; i++ {
		endpoints = append(endpoints, endpoint)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !client.TestEndpoints(endpoints) {
				t.Error("valid local ping endpoints rejected")
			}
		}()
	}
	wg.Wait()
	oldConnection := client.conns[0]
	if err := client.UpdateConfig(&configs.APIConfig{NodeId: "original-node", Secret: "0123456789abcdef0123456789abcdef", RPCEndpoints: []string{endpoint}}); err != nil {
		t.Fatal(err)
	}
	if oldConnection.GetState() != connectivity.Shutdown {
		t.Fatal("replaced RPC connections were leaked")
	}
}
