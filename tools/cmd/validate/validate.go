//
// Copyright (c) 2020-2021, NVIDIA CORPORATION. All rights reserved.
//
// See LICENSE.txt for license information
//

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/backtraces"
	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/counts"
	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/hash"
	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/location"
	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/profiler"
	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/timings"
	"github.com/gvallee/alltoallv_profiling/tools/internal/pkg/webui"
	"github.com/gvallee/go_util/pkg/util"
)

const (
	sharedLibCounts      = "liballtoallv_counts.so"
	sharedLibBacktrace   = "liballtoallv_backtrace.so"
	sharedLibLocation    = "liballtoallv_location.so"
	sharedLibLateArrival = "liballtoallv_late_arrival.so"
	sharedLibA2ATime     = "liballtoallv_exec_timings.so"

	exampleFileC          = "alltoallv.c"
	exampleFileDatatypeC  = "alltoallv_dt.c"
	exampleFileF          = "alltoallv.f90"
	exampleFileMulticommC = "alltoallv_multicomms.c"
	exampleFileBigCountsC = "alltoallv_bigcounts.c"

	exampleBinaryC          = "alltoallv_c"
	exampleBinaryF          = "alltoallv_f"
	exampleBinaryMulticommC = "alltoallv_multicomms_c"
	exampleBinaryBigCountsC = "alltoallv_bigcounts_c"
	exampleBinaryDatatypeC  = "alltoallv_dt_c"

	sharedLibAlltoAllUnequalCounts        = "liballtoall_counts_unequal.so"
	sharedLibAlltoAllUnequalCountsCompact = "liballtoall_counts_unequal_compact.so" // an extra one compared to alltoallv ones above
	sharedLibAlltoAllUnequalBacktrace     = "liballtoall_backtrace_counts_unequal.so"
	sharedLibAlltoAllUnequalLocation      = "liballtoall_location_counts_unequal.so"
	sharedLibAlltoAllUnequalLateArrival   = "liballtoall_late_arrival_counts_unequal.so"
	sharedLibAlltoAllUnequalA2ATime       = "liballtoall_exec_timings_counts_unequal.so"

	exampleFileAlltoallSimpleC    = "alltoall_simple_c.c" // TODO add some rows for other alltoall test programs - each will need a test struct below
	exampleFileAlltoallBigcountsC = "alltoall_bigcounts_c.c"
	exampleFileAlltoallMulticommC = "alltoall_multicomms_c.c"
	exampleFileAlltoallDatatypeC  = "alltoall_dt_c.c"

	exampleBinaryAlltoallSimpleC    = "alltoall_simple_c"
	exampleBinaryAlltoallBigcountsC = "alltoall_bigcounts_c"
	exampleBinaryAlltoallMulticommC = "alltoall_multicomms_c"
	exampleBinaryAlltoallDatatypeC  = "alltoall_dt_c"

	expectedIndexPageFile = "common_expected_index.html"

	noValidationStep              = 0
	allValidationSteps            = 1
	traceGenerationStep           = 2
	postmortemSRCountAnalyzerStep = 3
	postmortemProfilerStep        = 4
	webuiStep                     = 5
)

// Test gathers all the information required to run a specific test
type Test struct {
	collective                     string
	requestedValidationStepsToRun  []int
	validationStepsToRun           map[int]bool
	np                             int
	source                         string
	binary                         string
	totalNumCalls                  int
	numRanksPerComm                []int
	numCallsPerComm                []int
	expectedSendCompactCountsFiles []string
	expectedRecvCompactCountsFiles []string
	expectedCountsFiles            []string
	expectedLocationFiles          []string
	expectedExecTimeFiles          []string
	expectedLateArrivalFiles       []string
	expectedBacktraceFiles         []string
	profilerStepsToExecute         string
	listGraphsToGenerate           []string

	// Expected output from the postmortem analysis
	checkContentHeatMap      bool
	expectedSendHeatMapFiles []string
	expectedRecvHeatMapFiles []string
	expectedHostHeatMapFiles []string
}

type testCfg struct {
	tempDir string
	cfg     *Test
}

type validationCfg struct {
	sharedLibraries []string
	tests           []Test
	testCfgs        map[string]*testCfg
}

func (v *validationCfg) updateValidationStepsDependencies() {
	for _, tt := range v.tests {
		v.testCfgs[tt.binary].cfg.validationStepsToRun = make(map[int]bool)

		for _, step := range v.testCfgs[tt.binary].cfg.requestedValidationStepsToRun {
			if step == allValidationSteps {
				for i := 0; i <= webuiStep; i++ {
					v.testCfgs[tt.binary].cfg.validationStepsToRun[i] = true
				}
			}

			if step == webuiStep {
				v.testCfgs[tt.binary].cfg.validationStepsToRun[postmortemProfilerStep] = true
			}

			if step == postmortemProfilerStep || step == postmortemSRCountAnalyzerStep {
				v.testCfgs[tt.binary].cfg.validationStepsToRun[traceGenerationStep] = true
			}

			v.testCfgs[tt.binary].cfg.validationStepsToRun[step] = true
		}
	}
}

func validationStepIsSet(tt *Test, requestedStep int) bool {
	return tt.validationStepsToRun[requestedStep]
}

func validateCountProfiles(dir string, jobid int, id int) error {
	err := counts.Validate(jobid, id, dir)
	if err != nil {
		return err
	}

	return nil
}

// checkOutputFiles compares the file generated for the profiler with the save we expect to get.
// Files that we expect are stored in the repository in the 'tests' directory. We therefore
// assume that some of the output, the one check by this function, is always predictable.
func checkOutputFiles(expectedOutputDir string, tempDir string, expectedFiles []string) error {
	for _, expectedOutputFile := range expectedFiles {
		referenceFile := filepath.Join(expectedOutputDir, expectedOutputFile)
		resultFile := filepath.Join(tempDir, expectedOutputFile)
		fmt.Printf("- Comparing %s and %s...", referenceFile, resultFile)
		hashResultFile, err := hash.File(resultFile)
		if err != nil {
			fmt.Println(" failed")
			return err
		}
		hashRefFile, err := hash.File(referenceFile)
		if err != nil {
			fmt.Println(" failed")
			return err
		}
		if hashRefFile != hashResultFile {
			fmt.Println(" failed")
			return fmt.Errorf("invalid output, send counters do not match (%s vs. %s)", resultFile, referenceFile)
		}
		fmt.Println(" ok")
	}

	return nil
}

func checkFormatTimingFile(filepath string, codeBaseDir string, expectedNumCalls int, expectedNumRanks int, tt *Test) error {
	md, _, _, err := timings.ParseTimingFile(filepath, codeBaseDir)
	if err != nil {
		return fmt.Errorf("timings.ParseTimingFile(() failed: %s", err)
	}
	if md.NumCalls != expectedNumCalls {
		return fmt.Errorf("%s contains data for %d calls instead of %d", filepath, md.NumCalls, expectedNumCalls)
	}
	if md.NumRanks != expectedNumRanks {
		return fmt.Errorf("%s contains data for %d ranks instead of %d", filepath, md.NumRanks, expectedNumRanks)
	}
	return nil
}

func checkOutput(codeBaseDir string, tempDir string, tt *Test) error {
	expectedOutputDir := filepath.Join(codeBaseDir, "tests", tt.binary, "expectedOutput")

	fmt.Printf("Checking if %s exist(s)...\n", tt.expectedSendCompactCountsFiles)
	err := checkOutputFiles(expectedOutputDir, tempDir, tt.expectedSendCompactCountsFiles)
	if err != nil {
		return err
	}

	fmt.Printf("Checking if %s exist(s)...\n", tt.expectedRecvCompactCountsFiles)
	err = checkOutputFiles(expectedOutputDir, tempDir, tt.expectedRecvCompactCountsFiles)
	if err != nil {
		return err
	}

	fmt.Printf("Checking if %s exist(s)...\n", tt.expectedExecTimeFiles)
	index := 0
	for _, file := range tt.expectedExecTimeFiles {
		execTimingFile := filepath.Join(tempDir, file)
		if !util.FileExists(execTimingFile) {
			return fmt.Errorf("%s is missing", execTimingFile)
		}
		// We also check the format of the content
		err = checkFormatTimingFile(execTimingFile, codeBaseDir, tt.numCallsPerComm[index], tt.numRanksPerComm[index], tt)
		if err != nil {
			return err
		}
		index++
	}

	fmt.Printf("Checking if %s exist(s)...\n", tt.expectedLateArrivalFiles)
	index = 0
	for _, file := range tt.expectedLateArrivalFiles {
		lateArrivalFile := filepath.Join(tempDir, file)
		if !util.FileExists(lateArrivalFile) {
			return fmt.Errorf("%s is missing", lateArrivalFile)
		}
		// We also check the format of the content
		err = checkFormatTimingFile(lateArrivalFile, codeBaseDir, tt.numCallsPerComm[index], tt.numRanksPerComm[index], tt)
		if err != nil {
			return err
		}
		index++
	}

	fmt.Printf("Checking if %s exist(s)...\n", tt.expectedBacktraceFiles)
	index = 0
	for _, file := range tt.expectedBacktraceFiles {
		backtraceFile := filepath.Join(tempDir, file)
		if !util.FileExists(backtraceFile) {
			return fmt.Errorf("%s is missing", backtraceFile)
		}
		// The content of the backtraces is execution dependent so we cannot check the content against a template,
		// but we check the format by forcing the parsing of the files.
		_, err := backtraces.ReadBacktraceFile(codeBaseDir, backtraceFile, nil)
		if err != nil {
			return fmt.Errorf("%s's format is invalid: %s", backtraceFile, err)
		}
		index++
	}

	fmt.Printf("Checking if %s exist(s)...\n", tt.expectedLocationFiles)
	index = 0
	for _, file := range tt.expectedLocationFiles {
		locationFile := filepath.Join(tempDir, file)
		if !util.FileExists(locationFile) {
			return fmt.Errorf("%s is missing", locationFile)
		}
		// We also check the format of the content
		_, _, err := location.ParseLocationFile(codeBaseDir, locationFile)
		if err != nil {
			return fmt.Errorf("%s's format is invalid: %s", locationFile, err)
		}
		index++
	}

	return nil
}

func validateTestSRCountsAnalyzer(testName string, dir string) error {
	toolName := "srcountsanalyzer"
	_, filename, _, _ := runtime.Caller(0)
	basedir := filepath.Join(filepath.Dir(filename), "..", "..", "..")

	toolDir := filepath.Join(basedir, "tools", "cmd", toolName)
	toolBin := filepath.Join(toolDir, toolName)
	if !util.FileExists(toolBin) {
		return fmt.Errorf("%s does not exist", toolBin)
	}

	fmt.Printf("Running mostmortem analysis for %s in %s\n", testName, dir)
	cmd := exec.Command(toolBin, "-dir", dir, "-output-dir", dir, "-jobid", "0", "-rank", "0")
	err := cmd.Run()
	if err != nil {
		return err
	}

	expectedFiles := []string{"profile_alltoallv_job0.rank0.md",
		"stats-job0-rank0.md",
		"patterns-job0-rank0.md",
		"patterns-summary-job0-rank0.md"}

	expectedOutputDir := filepath.Join(basedir, "tests", testName, "expectedOutput")
	err = checkOutputFiles(expectedOutputDir, dir, expectedFiles)
	if err != nil {
		return err
	}

	return nil
}

func validateDatasetProfiler(codeBaseDir string, collectiveName string, testCfg *testCfg) error {
	for _, listGraphs := range testCfg.cfg.listGraphsToGenerate {
		postmortemCfg := profiler.PostmortemConfig{
			CodeBaseDir:    codeBaseDir,
			CollectiveName: collectiveName,
			Steps:          testCfg.cfg.profilerStepsToExecute,
			DatasetDir:     testCfg.tempDir,
			BinThresholds:  profiler.DefaultBinThreshold,
			SizeThreshold:  profiler.DefaultMsgSizeThreshold,
			CallsToPlot:    listGraphs,
		}
		err := postmortemCfg.Analyze()
		if err != nil {
			return err
		}
	}

	expectedOutputDir := filepath.Join(codeBaseDir, "tests", testCfg.cfg.binary, "expectedOutput")

	if testCfg.cfg.checkContentHeatMap {
		// Check whether the heap map files have been successfully created
		fmt.Printf("Checking if %s exist(s)...\n", testCfg.cfg.expectedSendHeatMapFiles)
		err := checkOutputFiles(expectedOutputDir, testCfg.tempDir, testCfg.cfg.expectedSendHeatMapFiles)
		if err != nil {
			return err
		}

		fmt.Printf("Checking if %s exist(s)...\n", testCfg.cfg.expectedRecvHeatMapFiles)
		err = checkOutputFiles(expectedOutputDir, testCfg.tempDir, testCfg.cfg.expectedRecvHeatMapFiles)
		if err != nil {
			return err
		}
	} else {
		for _, heatMapFile := range testCfg.cfg.expectedSendHeatMapFiles {
			expectedFile := filepath.Join(testCfg.tempDir, heatMapFile)
			if !util.FileExists(expectedFile) {
				return fmt.Errorf("expected file %s is missing", expectedFile)
			}
		}

		for _, heatMapFile := range testCfg.cfg.expectedRecvHeatMapFiles {
			expectedFile := filepath.Join(testCfg.tempDir, heatMapFile)
			if !util.FileExists(expectedFile) {
				return fmt.Errorf("expected file %s is missing", expectedFile)
			}
		}

	}

	// For the files for which the content cannot be predicted, we checks if the file exists
	// and we try to parse the file
	for _, heatMapFile := range testCfg.cfg.expectedHostHeatMapFiles {
		expectedFile := filepath.Join(testCfg.tempDir, heatMapFile)
		if !util.FileExists(expectedFile) {
			return fmt.Errorf("expected file %s is missing", expectedFile)
		}
	}

	return nil
}

func validateTestPostmortemResults(codeBaseDir string, collectiveName string, testCfg *testCfg) error {
	if validationStepIsSet(testCfg.cfg, postmortemSRCountAnalyzerStep) {
		err := validateTestSRCountsAnalyzer(testCfg.cfg.binary, testCfg.tempDir)
		if err != nil {
			return err
		}
	}

	if validationStepIsSet(testCfg.cfg, postmortemProfilerStep) {
		err := validateDatasetProfiler(codeBaseDir, collectiveName, testCfg)
		if err != nil {
			return err
		}
	}

	return nil
}

func (v *validationCfg) postmortemAnalysisTools(codeBaseDir string, collectiveName string) error {
	for source, testCfg := range v.testCfgs {
		if validationStepIsSet(testCfg.cfg, postmortemSRCountAnalyzerStep) || validationStepIsSet(testCfg.cfg, postmortemProfilerStep) {
			err := validateTestPostmortemResults(codeBaseDir, collectiveName, testCfg)
			if err != nil {
				fmt.Printf("validation of the postmortem analysis for %s in %s failed: %s\n", source, testCfg.tempDir, err)
				return err
			}
		}
	}

	return nil
}

func compareResultWithFileContent(filePath string, content string) (bool, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return false, err
	}
	expectedContent := string(data)

	if content != expectedContent {
		fmt.Printf("the content returned when accessing the page does not match expectation:\n%s\nvs.\n%s", content, expectedContent)
		return false, nil
	}
	return true, nil
}

func checkIndexPageContent(codeBaseDir string, content string) error {
	expectedFile := filepath.Join(codeBaseDir, "tests", expectedIndexPageFile)
	success, err := compareResultWithFileContent(expectedFile, content)
	if err != nil {
		return fmt.Errorf("unable to check the result: %s", err)
	}
	if !success {
		return fmt.Errorf("unexpected output")
	}
	return nil
}

func checkCallPageContent(codeBaseDir string, testCfg *testCfg, content string) error {
	expectedFile := filepath.Join(codeBaseDir, "tests", testCfg.cfg.binary, "expectedOutput", "call0.html")
	success, err := compareResultWithFileContent(expectedFile, content)
	if err != nil {
		return fmt.Errorf("unable to check the result: %s", err)
	}
	if !success {
		return fmt.Errorf("unexpected output")
	}
	return nil
}

func checkPatternsPageContent(codeBaseDir string, testCfg *testCfg, content string) error {
	expectedFile := filepath.Join(codeBaseDir, "tests", testCfg.cfg.binary, "expectedOutput", "patterns.html")
	success, err := compareResultWithFileContent(expectedFile, content)
	if err != nil {
		return fmt.Errorf("unable to check the result: %s", err)
	}
	if !success {
		return fmt.Errorf("unexpected output")
	}
	return nil
}

func validateIndexPage(codeBaseDir string, cfg *webui.Config) error {
	fmt.Printf("Validating index page...\n")
	url := fmt.Sprintf("http://localhost:%d", cfg.Port)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	bs := string(body)
	return checkIndexPageContent(codeBaseDir, bs)
}

func validateCallsPage(codeBaseDir string, cfg *webui.Config) error {
	fmt.Printf("Validating calls page...\n")
	url := fmt.Sprintf("http://localhost:%d/calls", cfg.Port)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// The list of calls for some test cases is very long so we are checking
	// anything at the moment, we just check that we do not get an error.
	return nil
}

func validateCallPage(codeBaseDir string, cfg *webui.Config, testCfg *testCfg) error {
	fmt.Printf("Validating call page...\n")
	url := fmt.Sprintf("http://localhost:%d/call?leadRank=0&callID=0", cfg.Port)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	bs := string(body)

	return checkCallPageContent(codeBaseDir, testCfg, bs)
}

func validatePatternsPage(codeBaseDir string, cfg *webui.Config, testCfg *testCfg) error {
	fmt.Printf("Validating call page...\n")
	url := fmt.Sprintf("http://localhost:%d/patterns", cfg.Port)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	bs := string(body)

	return checkPatternsPageContent(codeBaseDir, testCfg, bs)
}

func validateWebUIPages(codeBaseDir string, cfg *webui.Config, testCfg *testCfg) error {
	err := validateIndexPage(codeBaseDir, cfg)
	if err != nil {
		return err
	}

	err = validateCallsPage(codeBaseDir, cfg)
	if err != nil {
		return err
	}

	err = validateCallPage(codeBaseDir, cfg, testCfg)
	if err != nil {
		return err
	}

	err = validatePatternsPage(codeBaseDir, cfg, testCfg)
	if err != nil {
		return err
	}

	return nil
}

func validateWebUIForTest(codeBaseDir string, testCfg *testCfg, port int) error {
	//var stdout, stderr bytes.Buffer
	fmt.Printf("starting webui for %s on port %d...\n", testCfg.cfg.binary, port)

	cfg := webui.Init()
	cfg.Name = testCfg.cfg.binary
	cfg.DatasetDir = testCfg.tempDir
	cfg.Port = port
	err := cfg.Start()
	if err != nil {
		return err
	}
	defer func(cfg *webui.Config) error {
		fmt.Println("shutting the webui down")
		err = cfg.Stop()
		if err != nil {
			log.Printf("unable to cleanly stop webui: %s", err)
			return err
		}
		return nil
	}(cfg)

	time.Sleep(1 * time.Second)

	err = validateWebUIPages(codeBaseDir, cfg, testCfg)
	if err != nil {
		return err
	}

	time.Sleep(1 * time.Second)

	return nil
}

func (v *validationCfg) webUI(codeBaseDir string, collectiveName string) error {
	fmt.Println("- Validating the webUI")
	port := webui.DefaultPort

	for _, testCfg := range v.testCfgs {
		if validationStepIsSet(testCfg.cfg, webuiStep) {
			err := validateWebUIForTest(codeBaseDir, testCfg, port)
			if err != nil {
				return fmt.Errorf("validateWebUIForTest() failed: %s", err)
			}
			port++
		}
	}

	return nil
}

// profiler runs the profiler against examples and compare the resuls to the results output.
// If keepResults is set to true, the results are *not* removed after execution. They can then be used
// later on to validate postmortem analysis.
func (v *validationCfg) profiler(keepResults bool, fullValidation bool) error {
	_, filename, _, _ := runtime.Caller(0)
	codeBaseDir := filepath.Join(filepath.Dir(filename), "..", "..", "..")

	// Find MPI
	mpiBin, err := exec.LookPath("mpirun")
	if err != nil {
		return err
	}

	// Find make
	makeBin, err := exec.LookPath("make")
	if err != nil {
		return err
	}

	// Compile both the profiler libraries and the example
	log.Println("Building libraries and tests...")
	cmd := exec.Command(makeBin, "clean", "all")
	cmd.Dir = filepath.Join(codeBaseDir, "src", "alltoallv")
	err = cmd.Run()
	if err != nil {
		return err
	}

	cmd = exec.Command(makeBin, "clean", "all")
	cmd.Dir = filepath.Join(codeBaseDir, "examples")
	err = cmd.Run()
	if err != nil {
		return err
	}

	for testName, tt := range v.testCfgs {
		// Create a temporary directory where to store the results
		tempDir, err := ioutil.TempDir("", "")
		if err != nil {
			return err
		}
		v.testCfgs[testName].tempDir = tempDir

		// Run the profiler
		// todo: use https://github.com/gvallee/go_hpc_jobmgr so we can easilty validate on local machine and clusters
		if validationStepIsSet(tt.cfg, traceGenerationStep) {
			var stdout, stderr bytes.Buffer
			for _, lib := range v.sharedLibraries {
				pathToLib := filepath.Join(codeBaseDir, "src", tt.cfg.collective, lib)
				fmt.Printf("Running MPI application (%s) and gathering profiles with %s...\n", testName, pathToLib)
				cmd = exec.Command(mpiBin, "-np", strconv.Itoa(tt.cfg.np), "--oversubscribe", filepath.Join(codeBaseDir, "examples", testName))
				cmd.Env = append(os.Environ(),
					"LD_PRELOAD="+pathToLib,
					"A2A_PROFILING_OUTPUT_DIR="+tempDir)
				cmd.Dir = tempDir
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				err = cmd.Run()
				if err != nil {
					return fmt.Errorf("mpirun failed.\n\tstdout: %s\n\tstderr: %s", stdout.String(), stderr.String())
				}
			}

			// Check the results
			err = checkOutput(codeBaseDir, tempDir, tt.cfg)
			if err != nil {
				return err
			}

			// We clean up *only* when tests are successful and
			// if results do not need to be kept
			if !keepResults {
				os.RemoveAll(tempDir)
			}
		}
	}

	return nil
}

func main() {
	verbose := flag.Bool("v", false, "Enable verbose mode")
	counts := flag.Bool("counts", false, "Validate the count data generated during the validation run of the profiler with an MPI application. Requires the following additional options: -dir, -job, -id.")
	profilerValidation := flag.Bool("profiler", false, "Perform a validation of the profiler itself running various tests. Requires MPI. Does not require any additional option.")
	postmortemValidation := flag.Bool("postmortem", false, "Perform a validation of the postmortem analysis tools.")
	full := flag.Bool("full", true, "Run the full validation. WARNING! This may generate a huge amount of files and create file system issues!")
	dir := flag.String("dir", "", "Where all the data is")
	id := flag.Int("id", 0, "Identifier of the experiment, e.g., X from <pidX> in the profile file name")
	jobid := flag.Int("jobid", 0, "Job ID associated to the count files")
	help := flag.Bool("h", false, "Help message")
	webui := flag.Bool("webui", false, "Validate the WebUI")

	flag.Parse()

	defaultListGraphs := fmt.Sprintf("0-%d", profiler.DefaultNumGeneratedGraphs)
	bigListGraphs := "0-999"
	sharedLibraries := []string{sharedLibCounts, sharedLibBacktrace, sharedLibLocation, sharedLibLateArrival, sharedLibA2ATime,
		sharedLibAlltoAllUnequalCounts, sharedLibAlltoAllUnequalCountsCompact, sharedLibAlltoAllUnequalBacktrace,
		sharedLibAlltoAllUnequalLocation, sharedLibAlltoAllUnequalLateArrival, sharedLibAlltoAllUnequalA2ATime}
	validationTests := []Test{
		{
			collective:                     "alltoall",
			requestedValidationStepsToRun:  []int{traceGenerationStep},
			np:                             4,
			totalNumCalls:                  1,
			numCallsPerComm:                []int{1},
			numRanksPerComm:                []int{4},
			source:                         exampleFileAlltoallSimpleC,
			binary:                         exampleBinaryAlltoallSimpleC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoall_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoall_execution_times.rank0_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoall_late_arrival_times.rank0_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoall_backtrace_rank0_trace0.md"}, // TODO What about an entry for "alltoall_comm_data_rank0.md", "counts.rank0_call0.md" and "counts.rank0_call0.md"???
			//profilerStepsToExecute:         profiler.AllSteps,	//??? What is this
		},
		{
			collective:                     "alltoall",
			requestedValidationStepsToRun:  []int{traceGenerationStep},
			np:                             4,
			totalNumCalls:                  1000,
			numCallsPerComm:                []int{1000},
			numRanksPerComm:                []int{4},
			source:                         exampleFileAlltoallBigcountsC,
			binary:                         exampleBinaryAlltoallBigcountsC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoall_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoall_execution_times.rank0_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoall_late_arrival_times.rank0_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoall_backtrace_rank0_trace0.md"}, // TODO What about an entry for "alltoall_comm_data_rank0.md", "counts.rank0_call0.md" and "counts.rank0_call0.md"???
			//profilerStepsToExecute:         profiler.AllSteps,	//??? What is this
		},
		/* This test does not pass validation yet
		{
			collective:                     "alltoall",
			requestedValidationStepsToRun:  []int{traceGenerationStep},
			np:                             4,
			totalNumCalls:                  2,
			numCallsPerComm:                []int{1, 1}, // 1, 2, 2, 1, 1, 3, 3},
			numRanksPerComm:                []int{4, 3}, // 3, 3, 3, 2, 2, 2, 2},
			source:                         exampleFileAlltoallMulticommC,
			binary:                         exampleBinaryAlltoallMulticommC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt", "send-counters.job0.rank1.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt", "recv-counters.job0.rank1.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoall_locations_comm0_rank0.md", "alltoall_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoall_execution_times.rank0_comm0_job0.md", "alltoall_execution_times.rank1_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoall_late_arrival_times.rank0_comm0_job0.md", "alltoall_late_arrival_times.rank1_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoall_backtrace_rank0_trace0.md", "alltoall_backtrace_rank1_trace0.md"},
		},
		*/
		{
			collective:                     "alltoall",
			requestedValidationStepsToRun:  []int{traceGenerationStep},
			np:                             4,
			totalNumCalls:                  4,
			numCallsPerComm:                []int{4},
			numRanksPerComm:                []int{4},
			source:                         exampleFileAlltoallDatatypeC,
			binary:                         exampleBinaryAlltoallDatatypeC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoall_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoall_execution_times.rank0_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoall_late_arrival_times.rank0_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoall_backtrace_rank0_trace0.md"},
		},
		{
			collective:                     "alltoallv",
			requestedValidationStepsToRun:  []int{allValidationSteps},
			np:                             4,
			totalNumCalls:                  1,
			numCallsPerComm:                []int{1},
			numRanksPerComm:                []int{4},
			source:                         exampleFileC,
			binary:                         exampleBinaryC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoallv_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoallv_execution_times.rank0_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoallv_late_arrival_times.rank0_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoallv_backtrace_rank0_trace0.md"},
			profilerStepsToExecute:   profiler.AllSteps,
			checkContentHeatMap:      true,
			expectedSendHeatMapFiles: []string{"alltoallv_heat-map.rank0-send.md"},
			expectedRecvHeatMapFiles: []string{"alltoallv_heat-map.rank0-recv.md"},
			expectedHostHeatMapFiles: []string{"alltoallv_hosts-heat-map.rank0-recv.md", "alltoallv_hosts-heat-map.rank0-send.md"},
			listGraphsToGenerate:     []string{defaultListGraphs},
		},
		{
			collective:                     "alltoallv",
			requestedValidationStepsToRun:  []int{allValidationSteps},
			np:                             3,
			totalNumCalls:                  2,
			numCallsPerComm:                []int{2},
			numRanksPerComm:                []int{3},
			source:                         exampleFileF,
			binary:                         exampleBinaryF,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoallv_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoallv_execution_times.rank0_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoallv_late_arrival_times.rank0_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoallv_backtrace_rank0_trace0.md"},
			profilerStepsToExecute:   profiler.AllSteps,
			checkContentHeatMap:      true,
			expectedSendHeatMapFiles: []string{"alltoallv_heat-map.rank0-send.md"},
			expectedRecvHeatMapFiles: []string{"alltoallv_heat-map.rank0-recv.md"},
			expectedHostHeatMapFiles: []string{"alltoallv_hosts-heat-map.rank0-recv.md", "alltoallv_hosts-heat-map.rank0-send.md"},
			listGraphsToGenerate:     []string{defaultListGraphs},
		},
		{
			collective:                     "alltoallv",
			requestedValidationStepsToRun:  []int{allValidationSteps},
			np:                             4,
			totalNumCalls:                  3,
			numCallsPerComm:                []int{2, 1},
			numRanksPerComm:                []int{2, 4},
			source:                         exampleFileMulticommC,
			binary:                         exampleBinaryMulticommC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt", "send-counters.job0.rank2.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt", "recv-counters.job0.rank2.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoallv_locations_comm0_rank0.md", "alltoallv_locations_comm1_rank0.md", "alltoallv_locations_comm0_rank2.md"},
			expectedExecTimeFiles:    []string{"alltoallv_execution_times.rank0_comm0_job0.md", "alltoallv_execution_times.rank0_comm1_job0.md"},
			expectedLateArrivalFiles: []string{"alltoallv_late_arrival_times.rank0_comm0_job0.md", "alltoallv_late_arrival_times.rank0_comm1_job0.md"},
			expectedBacktraceFiles:   []string{"alltoallv_backtrace_rank0_trace0.md", "alltoallv_backtrace_rank0_trace1.md", "alltoallv_backtrace_rank0_trace2.md", "alltoallv_backtrace_rank2_trace0.md", "alltoallv_backtrace_rank2_trace1.md"},
			profilerStepsToExecute:   profiler.AllSteps,
			checkContentHeatMap:      true,
			expectedSendHeatMapFiles: []string{"alltoallv_heat-map.rank0-send.md", "alltoallv_heat-map.rank2-send.md"},
			expectedRecvHeatMapFiles: []string{"alltoallv_heat-map.rank0-recv.md", "alltoallv_heat-map.rank2-recv.md"},
			expectedHostHeatMapFiles: []string{"alltoallv_hosts-heat-map.rank0-recv.md", "alltoallv_hosts-heat-map.rank0-send.md", "alltoallv_hosts-heat-map.rank2-recv.md", "alltoallv_hosts-heat-map.rank2-send.md"},
			listGraphsToGenerate:     []string{defaultListGraphs},
		},
		{
			collective:                     "alltoallv",
			requestedValidationStepsToRun:  []int{allValidationSteps},
			np:                             4,
			totalNumCalls:                  2,
			numCallsPerComm:                []int{2},
			numRanksPerComm:                []int{4},
			source:                         exampleFileDatatypeC,
			binary:                         exampleBinaryDatatypeC,
			expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
			expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
			// todo: expectedCountsFiles
			expectedLocationFiles:    []string{"alltoallv_locations_comm0_rank0.md"},
			expectedExecTimeFiles:    []string{"alltoallv_execution_times.rank0_comm0_job0.md"},
			expectedLateArrivalFiles: []string{"alltoallv_late_arrival_times.rank0_comm0_job0.md"},
			expectedBacktraceFiles:   []string{"alltoallv_backtrace_rank0_trace0.md", "alltoallv_backtrace_rank0_trace1.md"},
			profilerStepsToExecute:   profiler.AllSteps,
			checkContentHeatMap:      true,
			expectedSendHeatMapFiles: []string{"alltoallv_heat-map.rank0-send.md"},
			expectedRecvHeatMapFiles: []string{"alltoallv_heat-map.rank0-recv.md"},
			expectedHostHeatMapFiles: []string{"alltoallv_hosts-heat-map.rank0-recv.md", "alltoallv_hosts-heat-map.rank0-send.md"},
			listGraphsToGenerate:     []string{defaultListGraphs},
		},
	}

	if *full {
		extaTests := []Test{
			{
				collective:                     "alltoallv",
				requestedValidationStepsToRun:  []int{allValidationSteps},
				np:                             4, // This test runs a large number of interations over a collective with a limited number of ranks
				totalNumCalls:                  1000000,
				numCallsPerComm:                []int{1000000},
				numRanksPerComm:                []int{4},
				source:                         exampleFileBigCountsC,
				binary:                         exampleBinaryBigCountsC,
				expectedSendCompactCountsFiles: []string{"send-counters.job0.rank0.txt"},
				expectedRecvCompactCountsFiles: []string{"recv-counters.job0.rank0.txt"},
				// todo: expectedCountsFiles
				expectedLocationFiles:    []string{"alltoallv_locations_comm0_rank0.md"},
				expectedExecTimeFiles:    []string{"alltoallv_execution_times.rank0_comm0_job0.md"},
				expectedLateArrivalFiles: []string{"alltoallv_late_arrival_times.rank0_comm0_job0.md"},
				expectedBacktraceFiles:   []string{"alltoallv_backtrace_rank0_trace0.md"},
				profilerStepsToExecute:   profiler.DefaultSteps,
				checkContentHeatMap:      false, // heat maps for that test are too big to be in the repo
				expectedSendHeatMapFiles: []string{"alltoallv_heat-map.rank0-send.md"},
				expectedRecvHeatMapFiles: []string{"alltoallv_heat-map.rank0-recv.md"},
				expectedHostHeatMapFiles: []string{"alltoallv_hosts-heat-map.rank0-recv.md", "alltoallv_hosts-heat-map.rank0-send.md"},
				listGraphsToGenerate:     []string{defaultListGraphs, bigListGraphs},
			},
		}
		validationTests = append(validationTests, extaTests...)
	}

	cmdName := filepath.Base(os.Args[0])
	if *help {
		fmt.Printf("%s validates various aspects of this infrastructure", cmdName)
		fmt.Println("\nUsage:")
		flag.PrintDefaults()
		os.Exit(0)
	}

	logFile := util.OpenLogFile("alltoallv", cmdName)
	defer logFile.Close()
	if *verbose {
		nultiWriters := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(nultiWriters)
	} else {
		log.SetOutput(ioutil.Discard)
	}

	if !*counts && !*profilerValidation && !*postmortemValidation && !*webui {
		fmt.Println("No valid option selected, run '-h' for more details")
		os.Exit(1)
	}

	_, filename, _, _ := runtime.Caller(0)
	codeBaseDir := filepath.Join(filepath.Dir(filename), "..", "..", "..")

	collectiveName := "alltoallv" // hardcoded for now, detection coming soon

	if *webui {
		*postmortemValidation = true
	}

	// Create a map to store the data about all the directories where
	// results are created when the results need to be kept
	validation := new(validationCfg)
	validation.tests = validationTests
	validation.sharedLibraries = sharedLibraries
	validation.testCfgs = make(map[string]*testCfg)

	for idx, tt := range validationTests {
		cfg := new(testCfg)
		cfg.cfg = &validationTests[idx]
		validation.testCfgs[tt.binary] = cfg
	}

	validation.updateValidationStepsDependencies()

	if *profilerValidation && !*postmortemValidation {
		err := validation.profiler(false, *full)
		if err != nil {
			fmt.Printf("Validation of the infrastructure failed: %s\n", err)
			os.Exit(1)
		}
	}

	if *postmortemValidation {
		err := validation.profiler(true, *full)
		if err != nil {
			fmt.Printf("Validation of the infrastructure failed: %s\n", err)
			os.Exit(1)
		}

		err = validation.postmortemAnalysisTools(codeBaseDir, collectiveName)
		if err != nil {
			fmt.Printf("Validation of the postmortem analysis tools failed: %s\n", err)
			os.Exit(1)
		}

		if *webui {
			err := validation.webUI(codeBaseDir, collectiveName)
			if err != nil {
				fmt.Printf("Validation of the WebUI failed: %s", err)
				os.Exit(1)
			}
		}

		// If successful, we can then delete all the directory that were created
		for _, cfg := range validation.testCfgs {
			os.RemoveAll(cfg.tempDir)
		}

	}

	if *counts {
		err := validateCountProfiles(*dir, *jobid, *id)
		if err != nil {
			fmt.Printf("Validation of the count data failed")
			os.Exit(1)
		}
	}

	fmt.Println("Successful validation")
}
