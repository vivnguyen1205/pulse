package main

import (
	"flag"
	"fmt"
	"os"

	"pulse/server/anomaly/backtest"
)

func main() {
	config := backtest.DefaultConfig()
	flag.IntVar(&config.Trials, "trials", config.Trials, "series per fault type per evaluation seed")
	flag.IntVar(&config.Seeds, "seeds", config.Seeds, "independent evaluation seeds")
	flag.IntVar(&config.CalibrationTrials, "calibration-trials", config.CalibrationTrials,
		"fault-free series used for threshold calibration")
	flag.Int64Var(&config.Seed, "seed", config.Seed, "base seed for evaluation series")
	flag.Int64Var(&config.CalibrationSeed, "calibration-seed", config.CalibrationSeed,
		"independent seed for calibration series")
	flag.Float64Var(&config.TargetFalseAlarmsPer1000, "target-false-alarms",
		config.TargetFalseAlarmsPer1000, "calibration target per 1,000 normal points")
	flag.Parse()

	report, err := backtest.Run(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "back-test failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Trials per fault and seed: %d; evaluation seeds: %v; calibration trials: %d; calibration seed: %d\n",
		config.Trials, report.EvaluationSeeds, config.CalibrationTrials, config.CalibrationSeed)
	fmt.Printf("Target: %.2f false alarm points per 1,000 normal points; warm-up: %d points; control horizon: %d points; fault horizon: %d points\n\n",
		config.TargetFalseAlarmsPer1000, config.BaselineSize, config.NormalPoints, config.FaultPoints)
	fmt.Print(backtest.MarkdownResults("Default settings", report.Defaults))
	fmt.Print("\n")
	fmt.Print(backtest.MarkdownControls("Default settings", report.DefaultControls))
	fmt.Print("\n")
	fmt.Print(backtest.MarkdownCalibration(report.Calibration))
	fmt.Print("\n")
	fmt.Print(backtest.MarkdownResults("Matched false-alarm rate", report.Matched))
	fmt.Print("\n")
	fmt.Print(backtest.MarkdownControls("Matched false-alarm rate", report.MatchedControls))
	fmt.Println("\nDetection requires the first alert after warm-up to fall within the fault-specific window.")
	fmt.Println("Step shifts: 12 points (1 hour); gradual drift: half a day; single spike: the injected point only.")
	fmt.Println("Episodes merge consecutive flagged normal points; rates are normalized to 1,000 normal points or one normal day.")
	fmt.Println("Rates show mean [minimum–maximum] across evaluation seeds; delay is the pooled median among timely detections [seed median minimum–maximum].")
}
