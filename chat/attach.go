package chat

import (
	"context"

	"github.com/mudler/nib/attachments"
	"github.com/mudler/nib/extraction"
	"github.com/mudler/nib/provenance"
	"github.com/mudler/nib/specialist"
)

// composeAttachments merges the preamble ahead of the user text and maps
// attachments.Part → chat.ContentPart. Pure/testable.
func composeAttachments(userText string, res attachments.Result) (string, []ContentPart) {
	text := res.TextPreamble + userText
	var parts []ContentPart
	for _, p := range res.Parts {
		k := PartImage
		audioFormat := ""
		switch p.Kind {
		case attachments.KindAudio:
			k = PartAudio
			audioFormat = p.Format // feeds input_audio.format; empty for image/video
		case attachments.KindVideo:
			k = PartVideo
		}
		parts = append(parts, ContentPart{Kind: k, DataURI: p.DataURI, AudioFormat: audioFormat})
	}
	return text, parts
}

// SendWithAttachments resolves treatments for files against the active model's
// capabilities, runs conversion/transcription, and sends one multimodal turn.
// Returns any blocked files (media the active model can't accept) for the caller
// to surface.
func (s *Session) SendWithAttachments(ctx context.Context, text string, files []string,
	overrides map[string]attachments.Override) (string, []attachments.Blocked, error) {

	caps := attachments.FetchCapabilities(ctx, s.baseURL, s.apiKey, s.Model())
	sp := specialist.New(s.baseURL, s.apiKey)

	extract := func(path string) (string, error) {
		if extraction.IsPlainTextFile(path) {
			return extraction.ReadTextFile(path)
		}
		return extraction.ExtractText(path)
	}
	transcribe := func(ctx context.Context, path string) (string, error) {
		return sp.Transcribe(ctx, path, s.transcribeModel)
	}

	res, err := attachments.Apply(ctx, files, caps, overrides, extract, transcribe)
	if err != nil {
		return "", nil, err
	}
	finalText, parts := composeAttachments(text, res)
	if res.TextPreamble != "" && s.provenanceClassifier != nil {
		e := provenance.NewExternal(ctx, "", "attachment", "user-selected-file", res.TextPreamble, s.provenanceClassifier)
		finalText = e.ModelText() + "\n" + text
		s.recordExternalEnvelope(e)
	}
	if finalText == "" && len(parts) == 0 {
		return "", res.Blocked, nil // nothing sendable (all blocked, no text)
	}
	reply, err := s.SendMessage(finalText, parts...)
	return reply, res.Blocked, err
}
