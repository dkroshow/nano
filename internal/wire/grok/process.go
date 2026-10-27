package grok

import "os/exec"

// ProcessConfig describes one short-lived ACP stdio client process.
type ProcessConfig struct {
	Command []string
	Dir     string
	Env     []string
}

// ProcessClient is the connection behavior needed to own an ACP child.
type ProcessClient interface {
	Start() error
	Done() <-chan struct{}
	Close() error
}

// StartProcess creates, configures, and starts one ACP stdio connection.
func StartProcess[T ProcessClient](config ProcessConfig, build func(*exec.Cmd) T, configure func(T)) (T, error) {
	var zero T
	if len(config.Command) == 0 || config.Command[0] == "" {
		return zero, exec.ErrNotFound
	}
	cmd := exec.Command(config.Command[0], config.Command[1:]...)
	cmd.Dir = config.Dir
	cmd.Env = config.Env
	client := build(cmd)
	configure(client)
	if err := client.Start(); err != nil {
		return zero, err
	}
	return client, nil
}

// WatchDisconnect waits for a client connection and reports its loss once.
func WatchDisconnect(client ProcessClient, disconnected func()) {
	<-client.Done()
	disconnected()
}
