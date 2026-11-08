package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/events"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/gw/eventgen"
)

func walkMessages(files []protoreflect.FileDescriptor, fn func(protoreflect.MessageDescriptor)) {
	var walk func(protoreflect.MessageDescriptors)
	walk = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			fn(ms.Get(i))
			walk(ms.Get(i).Messages())
		}
	}
	for _, f := range files {
		walk(f.Messages())
	}
}

// splitArgs parses fs over args, collecting positional arguments that
// may come before, between or after flags.
func splitArgs(fs *flag.FlagSet, args []string) ([]string, bool) {
	var files []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, false
		}
		if fs.NArg() == 0 {
			return files, true
		}
		files = append(files, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// schemaCheckCmd implements `gwag schema check`: compare every message
// with (gwag.ps.v1.shadow) against its registry subject.
func schemaCheckCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schema check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	regs := &stringListValue{}
	fs.Var(regs, "registry", "Registry NAME=URL for shadow options naming NAME (repeatable)")
	imports := &stringListValue{}
	fs.Var(imports, "I", "Import path (repeatable; default .)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag schema check --registry NAME=URL... [-I DIR]... FILE.proto...")
		fmt.Fprintln(fs.Output(), "  Checks each message with option (gwag.ps.v1.shadow) against its registry subject.")
		fmt.Fprintln(fs.Output(), "  side CONSUMER: kept fields must match number, type, cardinality, enum values; extra fields fail.")
		fmt.Fprintln(fs.Output(), "  side PRODUCER: the registry's compatibility API must accept the copy.")
		fmt.Fprintln(fs.Output(), "  "+registryEnvHelp)
		fs.PrintDefaults()
	}
	files, ok := splitArgs(fs, args)
	if !ok {
		return 2
	}
	if len(files) == 0 {
		fs.Usage()
		return 2
	}
	urls := map[string]string{}
	for _, r := range regs.values {
		name, u, ok := strings.Cut(r, "=")
		if !ok || name == "" || strings.Contains(name, ":") {
			fmt.Fprintf(stderr, "schema check: --registry %q: want NAME=URL\n", r)
			return 2
		}
		urls[name] = u
	}
	ctx := context.Background()
	fds, err := eventgen.Compile(ctx, imports.values, files)
	if err != nil {
		fmt.Fprintf(stderr, "schema check: %v\n", err)
		return 1
	}
	clients := map[string]*sr.Client{}
	failed, checked := 0, 0
	walkMessages(fds, func(md protoreflect.MessageDescriptor) {
		s, ok := sr.ShadowOf(md)
		if !ok {
			return
		}
		checked++
		lines, err := checkShadow(ctx, md, s, urls, clients)
		side := "consumer"
		if s.Producer {
			side = "producer"
		}
		version := s.Version
		if version == "" {
			version = "latest"
		}
		head := fmt.Sprintf("%s (%s) vs %s %s@%s", md.FullName(), side, s.Registry, s.Subject, version)
		var fails []string
		for _, l := range lines {
			if !l.Warning {
				fails = append(fails, l.String())
			}
		}
		switch {
		case err != nil:
			failed++
			fmt.Fprintf(stdout, "FAIL %s: %v\n", head, err)
		case len(fails) > 0:
			failed++
			fmt.Fprintf(stdout, "FAIL %s\n", head)
		default:
			fmt.Fprintf(stdout, "ok   %s\n", head)
		}
		for _, l := range lines {
			tag := "  error  "
			if l.Warning {
				tag = "  warning"
			}
			fmt.Fprintf(stdout, "%s %s\n", tag, l)
		}
	})
	if checked == 0 {
		fmt.Fprintln(stderr, "schema check: no message has option (gwag.ps.v1.shadow)")
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func checkShadow(ctx context.Context, md protoreflect.MessageDescriptor, s sr.Shadow, urls map[string]string, clients map[string]*sr.Client) ([]sr.Problem, error) {
	if s.Registry == "" || s.Subject == "" {
		return nil, fmt.Errorf("shadow option needs registry and subject")
	}
	u, ok := urls[s.Registry]
	if !ok {
		return nil, fmt.Errorf("no --registry %s=URL", s.Registry)
	}
	c := clients[s.Registry]
	if c == nil {
		var err error
		if c, err = registryClient(u); err != nil {
			return nil, err
		}
		clients[s.Registry] = c
	}
	return sr.CheckShadow(ctx, c, s, md)
}

// schemaPushCmd implements `gwag schema push`: register the protos of
// every named channel (own), with their imports as references.
func schemaPushCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schema push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	registry := fs.String("registry", "", "Schema Registry URL (required)")
	strategy := fs.String("strategy", "topic", "Subject name strategy: topic (<topic>-value), record (<message>), topic-record (<topic>-<message>)")
	normalize := fs.Bool("normalize", false, "Pass normalize=true to the registry")
	dry := fs.Bool("dry-run", false, "Only look the schemas up; exit 1 when one is not registered")
	topics := &stringListValue{}
	fs.Var(topics, "topic", "Topic of a channel, CHANNEL_KEY=TOPIC (repeatable; default the stream name EV_<pkg>_<Name>, the Kafka backend's default)")
	imports := &stringListValue{}
	fs.Var(imports, "I", "Import path (repeatable; default .)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag schema push --registry URL [--strategy S] [--topic KEY=TOPIC]... [-I DIR]... FILE.proto...")
		fmt.Fprintln(fs.Output(), "  Registers the file of each named channel's messages, imports as references (subject = import path).")
		fmt.Fprintln(fs.Output(), "  "+registryEnvHelp)
		fs.PrintDefaults()
	}
	files, ok := splitArgs(fs, args)
	if !ok {
		return 2
	}
	if *registry == "" || len(files) == 0 {
		fs.Usage()
		return 2
	}
	strat, err := sr.StrategyByName(*strategy)
	if err != nil {
		fmt.Fprintf(stderr, "schema push: %v\n", err)
		return 2
	}
	topicOf := map[string]string{}
	for _, t := range topics.values {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			fmt.Fprintf(stderr, "schema push: --topic %q: want CHANNEL_KEY=TOPIC\n", t)
			return 2
		}
		topicOf[k] = v
	}
	ctx := context.Background()
	fds, err := eventgen.Compile(ctx, imports.values, files)
	if err != nil {
		fmt.Fprintf(stderr, "schema push: %v\n", err)
		return 1
	}
	chans, err := eventgen.Channels(fds)
	if err != nil {
		fmt.Fprintf(stderr, "schema push: %v\n", err)
		return 1
	}
	if len(chans) == 0 {
		fmt.Fprintln(stderr, "schema push: no named channels in the files")
		return 1
	}
	c, err := registryClient(*registry)
	if err != nil {
		fmt.Fprintf(stderr, "schema push: %v\n", err)
		return 1
	}
	code := 0
	for _, ch := range chans {
		topic := topicOf[ch.Key]
		if topic == "" {
			topic = events.StreamName(ch.Key)
		}
		msgs := ch.Members
		if ch.Kind != eventgen.KindUnion {
			msgs = []protoreflect.MessageDescriptor{ch.Message}
		}
		done := map[string]bool{}
		for _, md := range msgs {
			subject := strat(topic, md)
			if done[subject] {
				continue
			}
			done[subject] = true
			r, err := c.EnsureFile(ctx, subject, md.ParentFile(), sr.RegisterOptions{Register: !*dry, Normalize: *normalize})
			if err != nil {
				fmt.Fprintf(stdout, "FAIL %s %s: %v\n", ch.Key, subject, err)
				code = 1
				continue
			}
			fmt.Fprintf(stdout, "ok   %s subject %s version %d id %d\n", ch.Key, r.Subject, r.Version, r.ID)
		}
	}
	return code
}
