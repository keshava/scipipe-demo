package main

import (
	"strconv"

	sp "github.com/scipipe/scipipe"
	spcomp "github.com/scipipe/scipipe/components"
)

func main() {
	sp.InitLogInfo()

	// ------------------------------------------------
	// Set up paths
	// ------------------------------------------------

	tmpDir := "tmp"
	appsDir := "data/apps"
	refDir := appsDir + "/pipeline_test/ref"
	origDataDir := appsDir + "/pipeline_test/data"
	dataDir := "data"

	// ----------------------------------------------------------------------------
	// Data Download part of the workflow
	// ----------------------------------------------------------------------------

	wf := sp.NewWorkflow("caw-preprocessing", 4)

	downloadApps := wf.NewProc("download_apps", "wget http://uppnex.se/apps.tar.gz -O {o:apps}")
	downloadApps.SetPathStatic("apps", dataDir+"/uppnex_apps.tar.gz")

	unzipApps := wf.NewProc("unzip_apps", "zcat {i:targz} > {o:tar}")
	unzipApps.SetPathReplace("targz", "tar", ".gz", "")
	unzipApps.In("targz").Connect(downloadApps.Out("apps"))

	unTarApps := wf.NewProc("untar_apps", "tar -xvf {i:tar} -C "+dataDir+" # {o:outdir}")
	unTarApps.SetPathStatic("outdir", dataDir+"/apps")
	unTarApps.In("tar").Connect(unzipApps.Out("tar"))

	// ----------------------------------------------------------------------------
	// Main Workflow
	// ----------------------------------------------------------------------------

	refFasta := refDir + "/human_g1k_v37_decoy.fasta"
	refIndex := refDir + "/human_g1k_v37_decoy.fasta.fai"

	fastqPaths1 := map[string][]string{} // Python: fastq_paths1 = { "" : [] }
	fastqPaths2 := map[string][]string{} // Python: fastq_paths2 = { "" : [] }

	indexes := map[string][]string{}
	indexes["normal"] = []string{"1", "2", "4", "7", "8"}     // Python: ["1","2","4","7","8"]
	indexes["tumor"] = []string{"1", "2", "3", "5", "6", "7"} // Python: ["1","2","3","5","6","7"]

	// Init some process "holders"
	markDuplicatesProcs := map[string]*sp.Process{}
	streamToSubstream := map[string]*spcomp.StreamToSubStream{}

	for i, sampleType := range []string{"normal", "tumor"} {
		si := strconv.Itoa(i)
		indexSource := NewParamSource(wf, "index_src_"+sampleType, indexes[sampleType]...)

		for _, idx := range indexes[sampleType] {
			fastqPaths1[sampleType] = append(fastqPaths1[sampleType], origDataDir+"/tiny_"+sampleType+"_L00"+idx+"_R1.fastq.gz")
			fastqPaths2[sampleType] = append(fastqPaths2[sampleType], origDataDir+"/tiny_"+sampleType+"_L00"+idx+"_R2.fastq.gz")
		}

		// --------------------------------------------------------------------------------
		// Align samples
		// --------------------------------------------------------------------------------
		readsFastQ1 := NewIPSource(wf, "reads_fastq1_"+sampleType, fastqPaths1[sampleType]...)
		readsFastQ2 := NewIPSource(wf, "reads_fastq2_"+sampleType, fastqPaths2[sampleType]...)

		alignSamples := wf.NewProc("align_samples_"+sampleType,
			"bwa mem -R \"@RG\tID:"+sampleType+"_{p:index}\tSM:"+sampleType+"\tLB:"+sampleType+"\tPL:illumina\" -B 3 -t 4 -M "+refFasta+" {i:reads1} {i:reads2}"+
				"| samtools view -bS -t "+refIndex+" - "+
				"| samtools sort - > {o:bam} # {i:appsdir}")
		alignSamples.In("reads1").Connect(readsFastQ1.Out())
		alignSamples.In("reads2").Connect(readsFastQ2.Out())
		alignSamples.In("appsdir").Connect(unTarApps.Out("outdir"))
		alignSamples.ParamInPort("index").Connect(indexSource.Out())

		sampleType := sampleType // needed to work around Go's funny behaviour of closures
		alignSamples.SetPathCustom("bam", func(t *sp.Task) string {
			return tmpDir + "/" + sampleType + "_" + t.Param("index") + ".bam"
		})

		// --------------------------------------------------------------------------------
		// Merge BAMs
		// --------------------------------------------------------------------------------

		streamToSubstream[sampleType] = spcomp.NewStreamToSubStream(wf, "stream_to_substream_"+sampleType)
		streamToSubstream[sampleType].In().Connect(alignSamples.Out("bam"))

		mergeBams := wf.NewProc("merge_bams_"+sampleType, "samtools merge -f {o:mergedbam} {i:bams:r: }")
		mergeBams.In("bams").Connect(streamToSubstream[sampleType].OutSubStream())
		mergeBams.SetPathStatic("mergedbam", tmpDir+"/"+sampleType+".bam")

		// --------------------------------------------------------------------------------
		// Mark Duplicates
		// --------------------------------------------------------------------------------

		markDuplicates := wf.NewProc("mark_dupes_"+sampleType,
			`java -Xmx15g -jar `+appsDir+`/picard-tools-1.118/MarkDuplicates.jar \
				INPUT={i:bam} \
				METRICS_FILE=`+tmpDir+`/`+sampleType+`_`+si+`.md.bam \
				TMP_DIR=`+tmpDir+` \
				ASSUME_SORTED=true \
				VALIDATION_STRINGENCY=LENIENT \
				CREATE_INDEX=TRUE \
				OUTPUT={o:bam}; \
				mv `+tmpDir+`/`+sampleType+`_`+si+`.md{.bam.tmp,}.bai;`)
		markDuplicates.SetPathStatic("bam", tmpDir+"/"+sampleType+"_"+si+".md.bam")
		markDuplicates.In("bam").Connect(mergeBams.Out("mergedbam"))
		// Save in map for later use
		markDuplicatesProcs[sampleType] = markDuplicates
	}

	// --------------------------------------------------------------------------------
	// Re-align Reads - Create Targets
	// --------------------------------------------------------------------------------

	realignCreateTargets := wf.NewProc("realign_create_targets",
		`java -Xmx3g -jar `+appsDir+`/gatk/GenomeAnalysisTK.jar -T RealignerTargetCreator  \
				-I {i:bamnormal} \
				-I {i:bamtumor} \
				-R `+refDir+`/human_g1k_v37_decoy.fasta \
				-known `+refDir+`/1000G_phase1.indels.b37.vcf \
				-known `+refDir+`/Mills_and_1000G_gold_standard.indels.b37.vcf \
				-nt 4 \
				-XL hs37d5 \
				-XL NC_007605 \
				-o {o:intervals}`)
	realignCreateTargets.SetPathStatic("intervals", tmpDir+"/tiny.intervals")
	realignCreateTargets.In("bamnormal").Connect(markDuplicatesProcs["normal"].Out("bam"))
	realignCreateTargets.In("bamtumor").Connect(markDuplicatesProcs["tumor"].Out("bam"))

	// --------------------------------------------------------------------------------
	// Re-align Reads - Re-align Indels
	// --------------------------------------------------------------------------------

	realignIndels := wf.NewProc("realign_indels",
		`java -Xmx3g -jar `+appsDir+`/gatk/GenomeAnalysisTK.jar -T IndelRealigner \
			-I {i:bamnormal} \
			-I {i:bamtumor} \
			-R `+refDir+`/human_g1k_v37_decoy.fasta \
			-targetIntervals {i:intervals} \
			-known `+refDir+`/1000G_phase1.indels.b37.vcf \
			-known `+refDir+`/Mills_and_1000G_gold_standard.indels.b37.vcf \
			-XL hs37d5 \
			-XL NC_007605 \
			-nWayOut '.real.bam' # {o:realbamnormal} {o:realbamtumor}`)
	realignIndels.SetPathReplace("bamnormal", "realbamnormal", ".bam", ".real.bam")
	realignIndels.SetPathReplace("bamtumor", "realbamtumor", ".bam", ".real.bam")
	realignIndels.In("intervals").Connect(realignCreateTargets.Out("intervals"))
	realignIndels.In("bamnormal").Connect(markDuplicatesProcs["normal"].Out("bam"))
	realignIndels.In("bamtumor").Connect(markDuplicatesProcs["tumor"].Out("bam"))

	// --------------------------------------------------------------------------------
	// Re-calibrate reads
	// --------------------------------------------------------------------------------

	for _, sampleType := range []string{"normal", "tumor"} {

		// Re-calibrate
		reCalibrate := wf.NewProc("recalibrate_"+sampleType,
			`java -Xmx3g -Djava.io.tmpdir=`+tmpDir+` -jar `+appsDir+`/gatk/GenomeAnalysisTK.jar -T BaseRecalibrator \
				-R `+refDir+`/human_g1k_v37_decoy.fasta \
				-I {i:realbam} \
				-knownSites `+refDir+`/dbsnp_138.b37.vcf \
				-knownSites `+refDir+`/1000G_phase1.indels.b37.vcf \
				-knownSites `+refDir+`/Mills_and_1000G_gold_standard.indels.b37.vcf \
				-nct 4 \
				-XL hs37d5 \
				-XL NC_007605 \
				-l INFO \
				-o {o:recaltable}`)
		reCalibrate.SetPathStatic("recaltable", tmpDir+"/"+sampleType+".recal.table")
		reCalibrate.In("realbam").Connect(realignIndels.Out("realbam" + sampleType))

		// Print reads
		printReads := wf.NewProc("print_reads_"+sampleType,
			`java -Xmx3g -jar `+appsDir+`/gatk/GenomeAnalysisTK.jar -T PrintReads \
				-R `+refDir+`/human_g1k_v37_decoy.fasta \
				-nct 4 \
				-I {i:realbam} \
				-XL hs37d5 \
				-XL NC_007605 \
				--BQSR {i:recaltable} \
				-o {o:recalbam};
				fname={o:recalbam};
				mv $fname ${fname%.bam.tmp.bai}.bai;`)
		printReads.SetPathStatic("recalbam", sampleType+".recal.bam")
		printReads.In("realbam").Connect(realignIndels.Out("realbam" + sampleType))
		printReads.In("recaltable").Connect(reCalibrate.Out("recaltable"))
	}

	wf.Run()
}

// ----------------------------------------------------------------------------
// Helper processes
// ----------------------------------------------------------------------------

// ParamSource will feed parameters on an out-port
type ParamSource struct {
	sp.BaseProcess
	params []string
}

// NewParamSource returns a new ParamSource
func NewParamSource(wf *sp.Workflow, name string, params ...string) *ParamSource {
	p := &ParamSource{
		BaseProcess: sp.NewBaseProcess(wf, name),
		params:      params,
	}
	p.InitParamOutPort(p, "out")
	return p
}

func (p *ParamSource) Out() *sp.ParamOutPort { return p.ParamOutPort("out") }

// Run runs the process
func (p *ParamSource) Run() {
	defer p.CloseAllOutPorts()
	for _, param := range p.params {
		p.Out().Send(param)
	}
}

// ----------------------------------------------------------------------------

type IPSource struct {
	sp.BaseProcess
	filePaths []string
}

func NewIPSource(wf *sp.Workflow, name string, filePaths ...string) *IPSource {
	p := &IPSource{
		BaseProcess: sp.NewBaseProcess(wf, name),
		filePaths:   filePaths,
	}
	p.InitOutPort(p, "out")
	return p
}

func (p *IPSource) Out() *sp.OutPort { return p.OutPort("out") }

func (p *IPSource) Run() {
	defer p.CloseAllOutPorts()
	for _, filePath := range p.filePaths {
		p.Out().Send(sp.NewFileIP(filePath))
	}
}
