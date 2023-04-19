/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package operator

import (
	"context"
	"log"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapio"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/injection/sharedmain"
	"knative.dev/pkg/logging"
)

// NewLogger returns a configured *zap.SugaredLogger. The logger is
// configured by the ConfigMap `config-logging` and live updates the level.
func NewLogger(ctx context.Context, componentName string, config *rest.Config, cmw *informer.InformedWatcher) *zap.SugaredLogger {
	ctx, startInformers := injection.EnableInjectionOrDie(logging.WithLogger(ctx, zap.NewNop().Sugar()), config)
	logger, atomicLevel := sharedmain.SetupLoggerOrDie(ctx, componentName)
	rest.SetDefaultWarningHandler(&logging.WarningHandler{Logger: logger})
	sharedmain.WatchLoggingConfigOrDie(ctx, cmw, logger, atomicLevel, componentName)
	startInformers()
	return logger
}

// ConfigureGlobalLoggers sets up any package-wide loggers like "log" or "klog" that are utilized by other packages
// to use the configured *zap.SugaredLogger from the context
func ConfigureGlobalLoggers(ctx context.Context) {
	klog.SetLogger(zapr.NewLogger(logging.FromContext(ctx).Desugar()))
	w := &zapio.Writer{Log: logging.FromContext(ctx).Desugar(), Level: zap.DebugLevel}
	log.SetFlags(0)
	log.SetOutput(w)
}

type ignoreDebugEventsSink struct {
	name string
	sink logr.LogSink
}

func (i ignoreDebugEventsSink) Init(ri logr.RuntimeInfo) {
	i.sink.Init(ri)
}
func (i ignoreDebugEventsSink) Enabled(level int) bool { return i.sink.Enabled(level) }
func (i ignoreDebugEventsSink) Info(level int, msg string, keysAndValues ...interface{}) {
	// ignore debug "events" logs
	if level == 1 && i.name == "events" {
		return
	}
	i.sink.Info(level, msg, keysAndValues...)
}
func (i ignoreDebugEventsSink) Error(err error, msg string, keysAndValues ...interface{}) {
	i.sink.Error(err, msg, keysAndValues...)
}
func (i ignoreDebugEventsSink) WithValues(keysAndValues ...interface{}) logr.LogSink {
	return i.sink.WithValues(keysAndValues...)
}
func (i ignoreDebugEventsSink) WithName(name string) logr.LogSink {
	return &ignoreDebugEventsSink{name: name, sink: i.sink.WithName(name)}
}

// ignoreDebugEvents wraps the logger with one that ignores any debug logs coming from a logger named "events".  This
// prevents every event we write from creating a debug log which spams the log file during scale-ups due to recording
// pod scheduling decisions as events for visibility.
func ignoreDebugEvents(logger logr.Logger) logr.Logger {
	return logr.New(&ignoreDebugEventsSink{sink: logger.GetSink()})
}
