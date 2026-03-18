package voice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

type WhisperTranscriber struct {
	modelPath string
	cliPath   string
}

func NewWhisperTranscriber(cfg config.WhisperConfig) (*WhisperTranscriber, error) {
	modelPath := cfg.ModelPath
	if modelPath == "" {
		return nil, fmt.Errorf("whisper model_path is required")
	}

	// Expand home dir
	if strings.HasPrefix(modelPath, "~/") {
		home, _ := os.UserHomeDir()
		modelPath = filepath.Join(home, modelPath[2:])
	}

	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("whisper model file not found: %w", err)
	}

	cliPath := cfg.CliPath
	if cliPath == "" {
		if path, err := exec.LookPath("whisper-cli"); err == nil {
			cliPath = path
		} else {
			return nil, fmt.Errorf("whisper-cli binary not found in PATH")
		}
	} else {
		if _, err := os.Stat(cliPath); err != nil {
			return nil, fmt.Errorf("whisper-cli binary not found at %q: %w", cliPath, err)
		}
	}

	// Check for ffmpeg (required for audio conversion)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg binary not found (required for whisper transcription): %w", err)
	}

	return &WhisperTranscriber{
		modelPath: modelPath,
		cliPath:   cliPath,
	}, nil
}

func (t *WhisperTranscriber) Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error) {
	logger.InfoCF("voice", "Whisper transcribing using CLI", map[string]any{"file": audioFilePath})

	// 1. Convert to 16kHz mono WAV using ffmpeg (whisper-cli requires 16kHz wav)
	wavPath := filepath.Join(os.TempDir(), fmt.Sprintf("whisper-%x.wav", os.Getpid()))
	defer os.Remove(wavPath)

	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", audioFilePath, "-ar", "16000", "-ac", "1", wavPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w (output: %s)", err, string(output))
	}

	// 2. Run whisper-cli
	// whisper-cli -m <model> -f <audio.wav>
	cmdCli := exec.CommandContext(ctx, t.cliPath, "-m", t.modelPath, "-f", wavPath)
	output, err := cmdCli.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("whisper-cli failed: %w (output: %s)", err, string(output))
	}

	// 3. Parse output to extract text (stripping timestamps if present)
	// whisper-cli output format is usually: [00:00:00.000 --> 00:00:05.000]   Hello world.
	text := parseWhisperOutput(string(output))

	return &TranscriptionResponse{
		Text: text,
	}, nil
}

func parseWhisperOutput(output string) string {
	var lines []string
	re := regexp.MustCompile(`^\[.*\]\s+(.*)`)

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Ignore log lines from ggml/whisper (they usually don't start with [timestamp])
		if !strings.HasPrefix(line, "[") {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) > 1 {
			lines = append(lines, strings.TrimSpace(matches[1]))
		} else {
			// Fallback if timestamp format is different but it starts with [
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, " ")
}

func (t *WhisperTranscriber) Name() string {
	return "whisper"
}

func (t *WhisperTranscriber) Close() {
	// Nothing to close for CLI
}
