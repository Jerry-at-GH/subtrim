package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"subtrim"
	"subtrim/native"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "subtrim:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: subtrim clean | optimize | compare [options] input.ass")
	}
	command := args[0]
	if command != "clean" && command != "optimize" && command != "compare" {
		return errors.New("command must be clean, optimize or compare")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var config native.Config
	var videoSize, output string
	var downscale int
	var exhaustive, exact, o1, o2 bool
	var budget time.Duration
	flags.StringVar(&output, "output", "", "new output path; compare writes failed timestamps as CSV; never overwrites")
	if command == "optimize" {
		flags.DurationVar(&budget, "budget", 30*time.Second, "cooperative time budget; 0 is unlimited")
		flags.BoolVar(&o1, "O1", false, "use O1 optimization (default)")
		flags.BoolVar(&o2, "O2", false, "use O2 optimization")
	}
	if command != "clean" {
		flags.StringVar(&videoSize, "video-size", "", "video size; default ASS LayoutRes, then PlayRes, then 1920x1080")
		flags.IntVar(&downscale, "downscale", 3, "divide video width and height by this positive integer")
		flags.StringVar(&config.Library, "libass", "", "libass shared library path")
		flags.StringVar(&config.Fonts, "fonts", "", "font directory; disables system fonts, always uses embedded fonts")
		flags.BoolVar(&exhaustive, "exhaustive", false, "check every active integer millisecond instead of samples")
	}
	if command == "compare" {
		flags.BoolVar(&exact, "exact", false, "require zero pixel error instead of approximate tolerance")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if o1 && o2 {
		return errors.New("choose either -O1 or -O2")
	}
	expected := 1
	if command == "compare" {
		expected = 2
	}
	if flags.NArg() != expected {
		return fmt.Errorf("%s expects %d input path(s) after the options", command, expected)
	}
	if budget < 0 {
		return errors.New("budget must be nonnegative")
	}
	data, err := os.ReadFile(flags.Arg(0))
	if err != nil {
		return err
	}
	if command != "clean" {
		var dimensions [2]int
		if videoSize == "" {
			dimensions = subtrim.VideoSize(data)
		} else {
			w, h, ok := strings.Cut(videoSize, "x")
			if !ok {
				return errors.New("--video-size must be widthxheight")
			}
			dimensions[0], err = strconv.Atoi(w)
			if err != nil {
				return err
			}
			dimensions[1], err = strconv.Atoi(h)
			if err != nil {
				return err
			}
		}
		if downscale < 1 {
			return errors.New("--downscale must be a positive integer")
		}
		if dimensions[0] < downscale || dimensions[1] < downscale {
			return errors.New("video dimensions must be at least --downscale on each axis")
		}
		config.StorageWidth, config.StorageHeight = dimensions[0], dimensions[1]
		config.Width, config.Height = dimensions[0]/downscale, dimensions[1]/downscale
	}
	if command != "compare" {
		if output == "" {
			return errors.New("--output is required")
		}
		var result []byte
		var report subtrim.Report
		if command == "optimize" {
			level := "O1"
			if o2 {
				level = "O2"
			}
			result, report, err = subtrim.Optimize(ctx, data, subtrim.Options{Level: level, Config: config, Budget: budget, Exhaustive: exhaustive})
			if err != nil {
				return err
			}
		} else {
			start := time.Now()
			result = subtrim.Clean(data)
			report.ElapsedSeconds = time.Since(start).Seconds()
		}
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(result)
		closeErr := file.Close()
		if err = errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		label, coverage := command, "static"
		if report.Level != "" {
			label += " " + report.Level
		}
		switch report.FinalVerification.Coverage {
		case "sampled":
			coverage = "sampled"
		case "exhaustive_integer_ms":
			coverage = "exhaustive"
		}
		percent := 0.0
		if len(data) > 0 {
			percent = 100 * float64(len(data)-len(result)) / float64(len(data))
		}
		fmt.Printf("%s: saved %d bytes (%.1f%%); %.3fs", label, len(data)-len(result), percent, report.ElapsedSeconds)
		if command == "optimize" {
			fmt.Printf("; render %dx%d; %s", config.Width, config.Height, coverage)
		}
		switch report.Status {
		case "time_budget_exhausted":
			fmt.Print("; time budget exhausted")
		case "unsupported":
			fmt.Print("; unsupported format")
		case "coverage_limit":
			fmt.Print("; validation limit reached")
		case "no_visible_evidence":
			fmt.Print("; no visible rendering evidence")
		}
		_, err = fmt.Fprintln(os.Stdout)
		return err
	}
	afterData, err := os.ReadFile(flags.Arg(1))
	if err != nil {
		return err
	}
	before, err := native.Open(data, config)
	if err != nil {
		return err
	}
	defer before.Close()
	after, err := native.Open(afterData, config)
	if err != nil {
		return err
	}
	defer after.Close()
	var sampler native.Sampler
	times, err := sampler.Timeline(ctx, slices.Concat(before.Events, after.Events), exhaustive, 0)
	if err != nil {
		return err
	}
	maxError, maxMean := 2.0, 0.25
	if exact {
		maxError, maxMean = 0, 0
	}
	var failed func(int64, native.Metrics) error
	var file *os.File
	var writer *csv.Writer
	reportComplete := false
	if output != "" {
		file, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
		if err != nil {
			return err
		}
		defer func() {
			if !reportComplete {
				file.Close()
				os.Remove(output)
			}
		}()
		writer = csv.NewWriter(file)
		if err := writer.Write([]string{"timestamp_ms", "max_channel_error", "active_mean_error"}); err != nil {
			return err
		}
		failed = func(timestamp int64, metrics native.Metrics) error {
			return writer.Write([]string{strconv.FormatInt(timestamp, 10), strconv.FormatFloat(metrics.MaxChannelError, 'g', -1, 64), strconv.FormatFloat(metrics.ActiveMeanError, 'g', -1, 64)})
		}
	}
	result, err := native.CompareAll(ctx, before, after, times, maxError, maxMean, failed)
	if err != nil {
		return err
	}
	if writer != nil {
		writer.Flush()
		if err := errors.Join(writer.Error(), file.Close()); err != nil {
			return err
		}
		reportComplete = true
		fmt.Fprintf(os.Stdout, "failed timestamps: %s\n", output)
	}
	coverage := "sampled"
	if exhaustive {
		coverage = "exhaustive"
	}
	if !result.MeetsTolerance {
		return fmt.Errorf("rendering tolerance exceeded (%s, %d timestamps, render %dx%d)\n%d timestamps exceeded tolerance; first %d ms; last %d ms\nmax channel error %.6g/255; max active-pixel mean error %.6g/255", coverage, result.CheckedTimestamps, config.Width, config.Height, result.FailedTimestamps, *result.FirstExceededMS, *result.LastExceededMS, result.MaxChannelError, result.MaxActiveMeanError)
	}
	message := "within tolerance"
	if exact {
		message = "identical pixels"
	}
	_, err = fmt.Fprintf(os.Stdout, "%s (%s, %d timestamps, render %dx%d)\n", message, coverage, result.CheckedTimestamps, config.Width, config.Height)
	return err
}
