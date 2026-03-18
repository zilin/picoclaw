package googlechat

import (
	"encoding/json"
	"testing"
)

func TestGoogleChatMessage_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantCount int
	}{
		{
			name: "plural attachments",
			json: `{"text": "hi", "attachments": [{"contentName": "img1.png"}]}`,
			wantCount: 1,
		},
		{
			name: "singular attachment",
			json: `{"text": "hi", "attachment": [{"contentName": "img2.png"}]}`,
			wantCount: 1,
		},
		{
			name: "both provided (plural wins)",
			json: `{"text": "hi", "attachments": [{"contentName": "img1.png"}], "attachment": [{"contentName": "img2.png"}]}`,
			wantCount: 1,
		},
		{
			name: "none provided",
			json: `{"text": "hi"}`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg GoogleChatMessage
			if err := json.Unmarshal([]byte(tt.json), &msg); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}
			if len(msg.Attachments) != tt.wantCount {
				t.Errorf("expected %d attachments, got %d", tt.wantCount, len(msg.Attachments))
			}
			if tt.wantCount > 0 && msg.Attachments[0] == nil {
				t.Errorf("attachment should not be nil")
			}
		})
	}
}
