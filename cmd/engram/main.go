// Command engram publishes to, and checks, engram release channels.
//
//	engram publish  --bucket B --kms alias/K --project acme --channel dev \
//	                --commit <40 hex> --version V --time <RFC 3339> \
//	                acme/linux/amd64=dist/acme_linux_amd64.tar.gz ...
//	engram scrap    --bucket B --project acme --channel dev --commit <40 hex> --reason broken
//	engram pubkey   --kms alias/K [--identity release@example.org]
//	engram verify   --url https://pkg.example.org --project acme --channel dev \
//	                --signers allowed_signers [--commit <40 hex>] [--fetch]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/neuroplastio/engram"
	"github.com/neuroplastio/engram/signer"
	"github.com/neuroplastio/engram/sigv4"
	"github.com/neuroplastio/engram/sshsig"
	"github.com/neuroplastio/engram/store"
)

// Set at build time: -ldflags "-X main.version=… -X main.commit=…".
var version, commit = "dev", "unknown"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if os.Args[1] == "version" || os.Args[1] == "--version" {
		fmt.Printf("engram %s (%s)\n", version, commit)
		return
	}
	run, ok := map[string]func(context.Context, []string) error{
		"publish": publish, "scrap": scrap, "pubkey": pubkey, "verify": verify,
	}[os.Args[1]]
	if !ok {
		usage()
	}
	if err := run(context.Background(), os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "engram:", strings.TrimPrefix(err.Error(), "engram: "))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: engram publish|scrap|pubkey|verify|version [flags]   (-h on any of them)")
	os.Exit(2)
}

// channelFlags are the flags every subcommand that touches a channel takes.
type channelFlags struct{ project, channel string }

func (c *channelFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&c.project, "project", "", "project, e.g. acme")
	fs.StringVar(&c.channel, "channel", "", "channel, e.g. dev")
}

func (c *channelFlags) check() error {
	if c.project == "" || c.channel == "" {
		return errors.New("--project and --channel are required")
	}
	return nil
}

// storeFlags pick where the channel lives: a bucket of any S3-compatible
// service, or a directory for a dry run.
type storeFlags struct{ bucket, dir, region, endpoint string }

func (s *storeFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&s.bucket, "bucket", "", "S3 bucket holding the channels")
	fs.StringVar(&s.endpoint, "endpoint", "", "S3 endpoint, for a service that is not AWS (R2, MinIO, …)")
	fs.StringVar(&s.region, "region", "us-east-1", "region of the bucket, and of a KMS key")
	fs.StringVar(&s.dir, "dir", "", "directory to publish into instead of a bucket (dry run)")
}

func (s *storeFlags) open() (store.Store, error) {
	switch {
	case s.dir != "" && s.bucket == "":
		return store.Dir(s.dir), nil
	case s.bucket != "" && s.dir == "":
		creds, err := sigv4.FromEnv()
		if err != nil {
			return nil, err
		}
		return &store.S3{Bucket: s.bucket, Region: s.region, Endpoint: s.endpoint, Credentials: creds}, nil
	}
	return nil, errors.New("give exactly one of --bucket and --dir")
}

// keyFlags pick where the signing key lives.
type keyFlags struct{ kms, file string }

func (k *keyFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&k.kms, "kms", "", "sign with this AWS KMS key (id, ARN or alias/name)")
	fs.StringVar(&k.file, "key-file", "", "sign with this unencrypted OpenSSH Ed25519 private key instead")
}

func (k *keyFlags) open(ctx context.Context, region string) (sshsig.Signer, error) {
	switch {
	case k.file != "" && k.kms == "":
		return signer.OpenFile(k.file)
	case k.kms != "" && k.file == "":
		creds, err := sigv4.FromEnv()
		if err != nil {
			return nil, err
		}
		return signer.OpenKMS(ctx, signer.KMS{KeyID: k.kms, Region: region, Credentials: creds})
	}
	return nil, errors.New("give exactly one of --kms and --key-file")
}

func publish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	var ch channelFlags
	var st storeFlags
	var key keyFlags
	ch.add(fs)
	st.add(fs)
	key.add(fs)
	commit := fs.String("commit", "", "full 40-character commit of the build")
	version := fs.String("version", "", "version, for people")
	when := fs.String("time", "", "the commit's time, RFC 3339")
	minEnboot := fs.Int("min-enboot", 1, "lowest enboot protocol the build can be handed to")
	retention := fs.Duration("retention", 0, "expire builds superseded for longer than this (0 keeps everything), e.g. 720h")
	fs.Parse(args)

	if err := ch.check(); err != nil {
		return err
	}
	t, err := time.Parse(time.RFC3339, *when)
	if err != nil {
		return fmt.Errorf("--time: %w", err)
	}
	var uploads []engram.Upload
	for _, a := range fs.Args() {
		what, local, ok := strings.Cut(a, "=")
		parts := strings.Split(what, "/")
		if !ok || len(parts) != 3 {
			return fmt.Errorf("artifact %q is not name/os/arch=path", a)
		}
		uploads = append(uploads, engram.Upload{Name: parts[0], OS: parts[1], Arch: parts[2], Local: local})
	}
	s, err := st.open()
	if err != nil {
		return err
	}
	sg, err := key.open(ctx, st.region)
	if err != nil {
		return err
	}

	p := &engram.Publisher{Store: s, Signer: sg, Project: ch.project, Channel: ch.channel, Retention: *retention}
	b, err := p.Publish(ctx, engram.Build{Commit: *commit, Version: *version, Time: t, MinEnboot: *minEnboot}, uploads)
	if errors.Is(err, engram.ErrPublished) {
		// A re-run of a release workflow. The journal already has it.
		fmt.Printf("%s/%s: %s is already published\n", ch.project, ch.channel, *commit)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s: published %s (%s) at sequence %d, %d artifacts\n", ch.project, ch.channel, b.Version, b.Commit, b.Seq, len(uploads))
	return nil
}

func scrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scrap", flag.ExitOnError)
	var ch channelFlags
	var st storeFlags
	ch.add(fs)
	st.add(fs)
	commit := fs.String("commit", "", "full 40-character commit to withdraw")
	reason := fs.String("reason", "", "broken, security or mistake")
	fs.Parse(args)

	if err := ch.check(); err != nil {
		return err
	}
	switch *reason {
	case "", engram.ReasonBroken, engram.ReasonSecurity, engram.ReasonMistake:
	default:
		return fmt.Errorf("--reason %q is not broken, security or mistake", *reason)
	}
	s, err := st.open()
	if err != nil {
		return err
	}
	p := &engram.Publisher{Store: s, Project: ch.project, Channel: ch.channel}
	if err := p.Scrap(ctx, *commit, *reason); err != nil {
		return err
	}
	fmt.Printf("%s/%s: scrapped %s\n", ch.project, ch.channel, *commit)
	return nil
}

func pubkey(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	var key keyFlags
	key.add(fs)
	region := fs.String("region", "us-east-1", "region of a KMS key")
	identity := fs.String("identity", "", "print an allowed_signers line for this identity instead of a bare key")
	fs.Parse(args)

	k, err := key.open(ctx, *region)
	if err != nil {
		return err
	}
	if *identity != "" {
		fmt.Print(sshsig.AllowedSigners(*identity, engram.Namespace, k.Public()))
		return nil
	}
	fmt.Println(sshsig.AuthorizedKey(k.Public()))
	return nil
}

func verify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var ch channelFlags
	ch.add(fs)
	url := fs.String("url", "", "where the channels are served, e.g. https://pkg.neuroplast.io")
	signers := fs.String("signers", "", "allowed_signers file, or a file of OpenSSH public keys")
	commit := fs.String("commit", "", "verify this commit instead of the head")
	fetch := fs.Bool("fetch", false, "also download every artifact and check it")
	fs.Parse(args)

	if err := ch.check(); err != nil {
		return err
	}
	data, err := os.ReadFile(*signers)
	if err != nil {
		return fmt.Errorf("--signers: %w", err)
	}
	keys, err := sshsig.ParseAllowedSigners(data)
	if err != nil {
		return err
	}
	c := &engram.Client{Base: *url, Project: ch.project, Channel: ch.channel, Keys: keys}

	var m *engram.Manifest
	if *commit != "" {
		m, err = c.Manifest(ctx, *commit)
	} else if m, err = c.Latest(ctx, 0); err == nil && m == nil {
		return errors.New("the channel has no live build")
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s: %s (%s), sequence %d — signature good\n", m.Project, m.Channel, m.Build.Version, m.Build.Commit, m.Build.Seq)
	for _, a := range m.Artifacts {
		status := ""
		if *fetch {
			if _, err := c.Download(ctx, m, a); err != nil {
				return err
			}
			status = "  ok"
		}
		fmt.Printf("  %-14s %-7s %-6s %10d  %s%s\n", a.Name, a.OS, a.Arch, a.Size, a.Path, status)
	}
	return nil
}
