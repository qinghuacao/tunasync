package worker

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tuna/tunasync/internal"
)

type twoStageRsyncConfig struct {
	name                                         string
	rsyncCmd                                     string
	stage1Profile                                string
	upstreamURL, username, password, excludeFile string
	extraOptions                                 []string
	rsyncEnv                                     map[string]string
	workingDir, logDir, logFile                  string
	useIPv6                                      bool
	interval                                     time.Duration
	retry                                        int
}

// An RsyncProvider provides the implementation to rsync-based syncing jobs
type twoStageRsyncProvider struct {
	baseProvider
	twoStageRsyncConfig
	stage1Options []string
	stage2Options []string
	dataSize      string
}

var rsyncStage1Profiles = map[string]([]string){
	"debian": []string{"dists/"},
	"debian-oldstyle": []string{
		"Packages*", "Sources*", "Release*",
		"InRelease", "i18n/*", "ls-lR*", "dep11/*",
	},
}

func newTwoStageRsyncProvider(c twoStageRsyncConfig) (*twoStageRsyncProvider, error) {
	// TODO: check config options
	if !strings.HasSuffix(c.upstreamURL, "/") {
		return nil, errors.New("rsync upstream URL should ends with /")
	}
	if c.retry == 0 {
		c.retry = defaultMaxRetry
	}

	provider := &twoStageRsyncProvider{
		baseProvider: baseProvider{
			name:     c.name,
			ctx:      NewContext(),
			interval: c.interval,
			retry:    c.retry,
		},
		twoStageRsyncConfig: c,
		stage1Options: []string{
			"-aHvh", "--no-o", "--no-g", "--stats",
			"--exclude", ".~tmp~/",
			"--safe-links", "--timeout=120",
		},
		stage2Options: []string{
			"-aHvh", "--no-o", "--no-g", "--stats",
			"--exclude", ".~tmp~/",
			"--delete", "--delete-after", "--delay-updates",
			"--safe-links", "--timeout=120",
		},
	}

	if c.rsyncEnv == nil {
		provider.rsyncEnv = map[string]string{}
	}
	if c.username != "" {
		provider.rsyncEnv["USER"] = c.username
	}
	if c.password != "" {
		provider.rsyncEnv["RSYNC_PASSWORD"] = c.password
	}
	if c.rsyncCmd == "" {
		provider.rsyncCmd = "rsync"
	}

	provider.ctx.Set(_WorkingDirKey, c.workingDir)
	provider.ctx.Set(_LogDirKey, c.logDir)
	provider.ctx.Set(_LogFileKey, c.logFile)

	return provider, nil
}

func (p *twoStageRsyncProvider) Type() providerEnum {
	return provTwoStageRsync
}

func (p *twoStageRsyncProvider) Upstream() string {
	return p.upstreamURL
}

func (p *twoStageRsyncProvider) DataSize() string {
	return p.dataSize
}

func (p *twoStageRsyncProvider) Options(stage int) ([]string, error) {
	var options []string
	if stage == 1 {
		options = append(options, p.stage1Options...)
		stage1Excludes, ok := rsyncStage1Profiles[p.stage1Profile]
		if !ok {
			return nil, errors.New("Invalid Stage 1 Profile")
		}
		for _, exc := range stage1Excludes {
			options = append(options, "--exclude", exc)
		}

	} else if stage == 2 {
		options = append(options, p.stage2Options...)
		if p.extraOptions != nil {
			options = append(options, p.extraOptions...)
		}
	} else {
		return []string{}, fmt.Errorf("Invalid stage: %d", stage)
	}

	if p.useIPv6 {
		options = append(options, "-6")
	}

	if p.excludeFile != "" {
		options = append(options, "--exclude-from", p.excludeFile)
	}

	return options, nil
}

func (p *twoStageRsyncProvider) Run(started chan empty) error {
	p.Lock()
	defer p.Unlock()

	if p.IsRunning() {
		return errors.New("provider is currently running")
	}

	p.dataSize = ""
	stages := []int{1, 2}
	for _, stage := range stages {
		command := []string{p.rsyncCmd}
		options, err := p.Options(stage)
		if err != nil {
			return err
		}
		command = append(command, options...)
		command = append(command, p.upstreamURL, p.WorkingDir())

		p.cmd = newCmdJob(p, command, p.WorkingDir(), p.rsyncEnv)
		if err := p.prepareLogFile(stage > 1); err != nil {
			return err
		}
		defer p.closeLogFile()

		if err = p.cmd.Start(); err != nil {
			return err
		}
		p.isRunning.Store(true)
		logger.Debugf("set isRunning to true: %s", p.Name())
		started <- empty{}

		p.Unlock()
		err = p.Wait()
		p.Lock()
		if err != nil {
			code, msg := internal.TranslateRsyncErrorCode(err)
			if code != 0 {
				logger.Debug("Rsync exitcode %d (%s)", code, msg)
				if p.logFileFd != nil {
					p.logFileFd.WriteString(msg + "\n")
				}
			}
			return err
		}
	}
	p.dataSize = internal.ExtractSizeFromRsyncLog(p.LogFile())
	return nil
}
