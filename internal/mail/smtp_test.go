package mail

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestCodeMessage(t *testing.T) {
	const code = "01234567"
	message, err := mail.ReadMessage(strings.NewReader(codeMessage("hello@example.com", "user@example.com", code)))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("Subject"); got != "Your Shortlog sign-in code" {
		t.Fatalf("subject = %q", got)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type = %q, %v", mediaType, err)
	}
	parts := multipart.NewReader(message.Body, params["boundary"])
	for i, wantType := range []string{"text/plain", "text/html"} {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		gotType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || gotType != wantType {
			t.Fatalf("part %d type = %q, %v", i, gotType, err)
		}
		body, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), code) || !strings.Contains(string(body), "10 minutes") || !strings.Contains(string(body), "ignore this email") {
			t.Fatalf("part %d missing sign-in details: %q", i, body)
		}
	}
	if _, err := parts.NextPart(); err != io.EOF {
		t.Fatalf("unexpected extra part: %v", err)
	}
}
