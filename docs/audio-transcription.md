# Inbound Audio Transcription

Claworc can enable inbound audio and voice-message transcription for every managed agent from **Settings → Misc → Voice-message transcription**.

## What enabling does

Claworc writes this static OpenClaw configuration:

```json5
{
  tools: {
    media: {
      models: [{
        type: "provider",
        provider: "openai",
        model: "gpt-4o-transcribe",
        capabilities: ["audio"],
      }],
      audio: { enabled: true },
    },
  },
}
```

It also gives the already-configured, agent-local OpenAI gateway origin the narrow private-network permission required for audio uploads. This does **not** allow agents to fetch arbitrary private network URLs.

## Lifecycle and performance

- The setting is **off by default**.
- It starts no plugin, sidecar, probe, or background worker.
- Existing running agents receive only a hot-reloadable `tools.media` config update: no container or Gateway restart.
- New agents include the setting in Claworc's existing initial OpenClaw config batch, rather than adding another startup command.

Audio attachments are sent to the managed OpenAI provider for transcription. An agent without a compatible managed `openai` provider will report a transcription error until one is configured.
