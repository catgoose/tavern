package backend

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessageEnvelopeJSON(t *testing.T) {
	env := MessageEnvelope{
		Topic: "orders",
		Data:  `{"id":1}`,
		Scope: "user:42",
	}
	data, err := json.Marshal(env)
	require.NoError(t, err)

	var decoded MessageEnvelope
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	require.Equal(t, env, decoded)
}

func TestMessageEnvelopeJSON_OmitEmptyScope(t *testing.T) {
	env := MessageEnvelope{
		Topic: "orders",
		Data:  "hello",
	}
	data, err := json.Marshal(env)
	require.NoError(t, err)
	require.NotContains(t, string(data), "scope")
}
