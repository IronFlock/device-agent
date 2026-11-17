package logging

import (
	"fmt"
	"io"
	"os"
	"reagent/common"
	"reagent/config"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
	"gopkg.in/natefinch/lumberjack.v2"
)

func SetupLogger(cliArgs *config.CommandLineArguments) {
	rollingLogFile := &lumberjack.Logger{
		Filename:   cliArgs.LogFileLocation,
		MaxSize:    100,
		MaxBackups: 2,
	}

	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	var writer io.Writer
	if cliArgs.PrettyLogging {
		consoleWriter := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "2006-01-02 15:04:05"}
		writer = io.MultiWriter(consoleWriter, rollingLogFile)
	} else {
		writer = rollingLogFile
	}

	logger := zerolog.New(writer).With().CallerWithSkipFrameCount(2).Timestamp().Logger()
	log.Logger = logger

	if cliArgs.Debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	prettyArgs, err := common.PrettyFormat(*cliArgs)
	if err != nil {
		prettyArgs = fmt.Sprintf("%+v", *cliArgs)
	}

	// The first line of each start, at info so that it is in the log at
	// every level: ironflock-cli's tests look for it. Keep the text.
	log.Info().Msgf("REAgent CLI Arguments: %s", prettyArgs)
}
