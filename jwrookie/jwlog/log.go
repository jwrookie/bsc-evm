package jwlog

import (
	"fmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	Log   *zap.Logger
	Sugar *zap.SugaredLogger
)

func init() {
	ws, level := getConfigLogArgs()
	encoderConf := zap.NewProductionEncoderConfig()
	encoderConf.EncodeTime = zapcore.ISO8601TimeEncoder
	encoder := zapcore.NewConsoleEncoder(encoderConf)
	log := zap.New(
		zapcore.NewCore(encoder, ws, zap.NewAtomicLevelAt(level)),
		zap.AddCaller(),
	)
	Log = log
	Sugar = log.Sugar()
}

func getConfigLogArgs() (zapcore.WriteSyncer, zapcore.Level) {
	level := zap.InfoLevel

	var syncers []zapcore.WriteSyncer

	logger := &lumberjack.Logger{
		Filename:   fmt.Sprintf("/root/%s", "evm.log"), // if logs dir not exist, it will be auto create
		MaxSize:    1000,
		MaxBackups: 100,
		MaxAge:     60,
		Compress:   false,
		LocalTime:  true,
	}

	syncers = append(syncers, zapcore.AddSync(logger))
	ws := zapcore.NewMultiWriteSyncer(syncers...)

	return ws, level
}
