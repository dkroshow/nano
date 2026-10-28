package testsupport

import "fmt"

const CodexDeltaTurnStarted = `{"method":"turn/started","params":{"threadId":"native-thread","turn":{"id":"stream-turn","status":"inProgress"}}}`
const CodexDeltaItemStarted = `{"method":"item/started","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"agent-1","type":"agentMessage","text":""}}}`

func CodexDeltaFrames(parts ...string) [][]byte {
	var frames [][]byte
	for _, part := range parts {
		frames = append(frames, []byte(fmt.Sprintf(`{"method":"item/agentMessage/delta","params":{"threadId":"native-thread","turnId":"stream-turn","itemId":"agent-1","delta":%q}}`, part)))
	}
	return frames
}

func CodexDeltaItemCompleted(text string) []byte {
	return []byte(fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"stream-turn","item":{"id":"agent-1","type":"agentMessage","text":%q}}}`, text))
}

func CodexDeltaTurnCompleted(status string) []byte {
	return []byte(fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"native-thread","turn":{"id":"stream-turn","status":%q}}}`, status))
}
