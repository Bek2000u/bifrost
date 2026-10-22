package fastgen

import (
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

var modelSpecs = map[string]fastgenModelSpec{
	"flower-image":                          {"image", 1, []string{"aspect_ratio"}},
	"gemini-nano-banana-2":                  {"image", 10, []string{"aspect_ratio"}},
	"nano-banana-2":                         {"image", 10, []string{"aspect_ratio", "seed", "generation_config"}},
	"nano-banana-2-lite":                    {"image", 10, []string{"aspect_ratio", "seed", "generation_config"}},
	"nano-banana-pro":                       {"image", 10, []string{"aspect_ratio", "seed", "generation_config"}},
	"openai-image":                          {"image", 10, []string{"aspect_ratio", "quality"}},
	"flow-video-fast":                       {"video", 3, []string{"aspect_ratio", "seed", "keyframes", "ultra", "generation_config"}},
	"flow-video-lite":                       {"video", 3, []string{"aspect_ratio", "seed", "keyframes"}},
	"flow-video-omni-flash":                 {"video", 7, []string{"aspect_ratio", "seed", "keyframes", "duration_seconds"}},
	"flow-video-quality":                    {"video", 2, []string{"aspect_ratio", "seed", "keyframes"}},
	"flow-video-ultra-lite":                 {"video", 3, []string{"aspect_ratio", "seed", "keyframes", "generation_config"}},
	"flower-video":                          {"video", 1, []string{"aspect_ratio"}},
	"flower-video-omni":                     {"video", 3, []string{"aspect_ratio"}},
	"aistudio/gemini-2.5-flash-preview-tts": {"audio", 0, []string{"voice", "temperature", "speaker_voices", "scene", "sample_context"}},
	"aistudio/gemini-2.5-pro-preview-tts":   {"audio", 0, []string{"voice", "temperature", "speaker_voices", "scene", "sample_context"}},
	"aistudio/gemini-3.1-flash-tts-preview": {"audio", 0, []string{"voice", "temperature", "speaker_voices", "scene", "sample_context"}},
	"aistudio/gemini-3.8-flash-lite-tts":    {"audio", 0, []string{"voice", "temperature", "speaker_voices", "scene", "sample_context"}},
	"aistudio/gemini-3.8-flash-tts":         {"audio", 0, []string{"voice", "temperature", "speaker_voices", "scene", "sample_context"}},
	"aistudio/gemini-3.5-transcribe":        {"text", 1, []string{"language_codes", "custom_vocabulary", "word_timestamps", "diarization", "smart"}},
}

func ToBifrostListModelsResponse(rows []FastgenModel, key schemas.Key, unfiltered bool) *schemas.BifrostListModelsResponse {
	pipeline := providerUtils.ListModelsPipeline{AllowedModels: key.Models, BlacklistedModels: key.BlacklistedModels, Aliases: key.Aliases, Unfiltered: unfiltered, ProviderKey: schemas.Fastgen, MatchFns: providerUtils.DefaultMatchFns()}
	response := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	if pipeline.ShouldEarlyExit() {
		return response
	}
	included := map[string]bool{}
	for _, row := range rows {
		if row.ID == "" || row.Available != nil && !*row.Available {
			continue
		}
		for _, result := range pipeline.FilterModel(row.ID) {
			model := schemas.Model{ID: string(schemas.Fastgen) + "/" + result.ResolvedID, Name: schemas.Ptr(row.DisplayName), ContextLength: row.InputTokenLimit, OwnedBy: schemas.Ptr("fastgen")}
			if result.AliasValue != "" {
				model.Alias = schemas.Ptr(result.AliasValue)
			}
			response.Data = append(response.Data, model)
			included[result.ResolvedID] = true
		}
	}
	response.Data = append(response.Data, pipeline.BackfillModels(included)...)
	return response
}
