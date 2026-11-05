package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBunClientHelloMatchesOpenCode225(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	records := make(chan []byte, 1)
	go func() {
		defer server.Close()
		header := make([]byte, 5)
		if _, err := io.ReadFull(server, header); err != nil {
			records <- nil
			return
		}
		record := make([]byte, binary.BigEndian.Uint16(header[3:]))
		if _, err := io.ReadFull(server, record); err != nil {
			records <- nil
			return
		}
		records <- record
	}()
	transport, err := NewBunTransport(BunOptions{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return client, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The receiver closes after capturing the real TLS record.
	if conn, err := transport.dial(ctx, "https", "localhost:443", "localhost"); err == nil {
		conn.Close()
		t.Fatal("capture receiver unexpectedly completed a TLS handshake")
	}
	record := <-records
	if len(record) < 39 || record[0] != 1 {
		t.Fatalf("invalid ClientHello: %x", record)
	}
	i := 38
	i += 1 + int(record[i])
	i += 2 + int(binary.BigEndian.Uint16(record[i:]))
	i += 1 + int(record[i])
	end := i + 2 + int(binary.BigEndian.Uint16(record[i:]))
	i += 2
	var signatures []uint16
	for i < end {
		kind := binary.BigEndian.Uint16(record[i:])
		length := int(binary.BigEndian.Uint16(record[i+2:]))
		if kind == 13 {
			data := record[i+4 : i+4+length]
			for j := 2; j < len(data); j += 2 {
				signatures = append(signatures, binary.BigEndian.Uint16(data[j:]))
			}
		}
		i += 4 + length
	}
	// SignatureAlgorithms from the official 2.2.5 ClientHello capture.
	want := []uint16{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0807, 0x0603, 0x0201}
	if !slices.Equal(signatures, want) {
		t.Fatalf("wire signature algorithms = %04x; official 2.2.5 = %04x", signatures, want)
	}
}

func TestBunRequestMatchesOpenCode225HeaderOrder(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	captured := make(chan string, 1)
	go func() {
		defer server.Close()
		var raw bytes.Buffer
		request, err := http.ReadRequest(bufio.NewReader(io.TeeReader(server, &raw)))
		if err != nil {
			captured <- err.Error()
			return
		}
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			captured <- err.Error()
			return
		}
		request.Body.Close()
		captured <- raw.String()
		io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
	}()
	transport, err := NewBunTransport(BunOptions{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return client, nil
	}, ResponseHeaderTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodPost, "http://localhost/inference/openai/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.2.5/cli")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("Content-Length", "999")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	want := "POST /inference/openai/v1/chat/completions HTTP/1.1\r\n" +
		"Authorization: Bearer public\r\nContent-Type: application/json\r\n" +
		"User-Agent: opencode/latest/2.2.5/cli\r\nx-opencode-client: cli\r\n" +
		"Connection: keep-alive\r\nAccept: */*\r\nHost: localhost\r\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\r\nContent-Length: 2\r\n\r\n{}"
	if got := <-captured; got != want {
		t.Fatalf("wire request = %q; official header order = %q", got, want)
	}
}
