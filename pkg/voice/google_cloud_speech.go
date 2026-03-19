package voice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"golang.org/x/oauth2/google"
)

type GoogleCloudSpeechTranscriber struct {
	projectID    string
	languageCode string
	model        string
	httpClient   *http.Client
}

func NewGoogleCloudSpeechTranscriber(cfg config.GoogleCloudSpeechConfig) (*GoogleCloudSpeechTranscriber, error) {
	logger.DebugCF("voice", "Creating Google Cloud Speech transcriber", map[string]any{"project_id": cfg.ProjectID})

	lang := cfg.LanguageCode
	if lang == "" {
		lang = "en-US" // Default
	}

	model := cfg.Model
	if model == "" {
		model = "default" // Default
	}

	var httpClient *http.Client
	// Use ADC (Application Default Credentials)
	var err error
	httpClient, err = google.DefaultClient(context.Background(), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		logger.WarnCF("voice", "Failed to create DefaultClient for ADC. Falling back to default HTTP client.", map[string]any{"error": err.Error()})
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	return &GoogleCloudSpeechTranscriber{
		projectID:    cfg.ProjectID,
		languageCode: lang,
		model:        model,
		httpClient:   httpClient,
	}, nil
}

func (t *GoogleCloudSpeechTranscriber) Name() string {
	return "google_cloud_speech"
}

func (t *GoogleCloudSpeechTranscriber) Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error) {
	logger.InfoCF("voice", "Google Cloud Speech transcribing", map[string]any{"file": audioFilePath})

	// 1. Convert to 16kHz mono WAV using ffmpeg (Google Cloud Speech supports LINEAR16)
	wavPath := filepath.Join(os.TempDir(), fmt.Sprintf("gcspeech-%x.wav", os.Getpid()))
	defer os.Remove(wavPath)

	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", audioFilePath, "-ar", "16000", "-ac", "1", wavPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w (output: %s)", err, string(output))
	}

	// 2. Read WAV file and base64 encode
	wavBytes, err := os.ReadFile(wavPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read converted audio file: %w", err)
	}

	encodedAudio := base64.StdEncoding.EncodeToString(wavBytes)

	// 3. Construct JSON Payload
	payload := map[string]any{
		"config": map[string]any{
			"encoding":        "LINEAR16",
			"sampleRateHertz": 16000,
			"languageCode":    t.languageCode,
			"model":           t.model,
		},
		"audio": map[string]any{
			"content": encodedAudio,
		},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request payload: %w", err)
	}

	// 4. Send Request
	url := "https://speech.googleapis.com/v1/speech:recognize"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.projectID != "" {
		req.Header.Set("X-Goog-User-Project", t.projectID)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send http request to Google Speech API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("google speech api error (status %d): %s", resp.StatusCode, string(respBytes))
	}

	// 5. Parse Response
	var speechResponse struct {
		Results []struct {
			Alternatives []struct {
				Transcript string  `json:"transcript"`
				Confidence float64 `json:"confidence"`
			} `json:"alternatives"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&speechResponse); err != nil {
		return nil, fmt.Errorf("failed to decode speech api response: %w", err)
	}

	// Concatenate all transcripts from results
	var textParts []string
	for _, res := range speechResponse.Results {
		if len(res.Alternatives) > 0 {
			textParts = append(textParts, res.Alternatives[0].Transcript)
		}
	}

	transcriptText := strings.Join(textParts, " ")
	transcriptText = strings.TrimSpace(transcriptText)

	logger.InfoCF("voice", "Google Cloud Speech transcription completed", map[string]any{
		"text_len": len(transcriptText),
	})

	return &TranscriptionResponse{
		Text: transcriptText,
	}, nil
}
