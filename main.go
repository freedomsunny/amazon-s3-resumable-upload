// 多线程并发断点续传上传/下载S3，支持支持Amazon S3, Ali OSS, Tencent COS, Google GCS 等兼容S3 API的对象存储
// 使用 ./s3trans -h 获取更多帮助信息
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/sqs"
	_ "github.com/mattn/go-sqlite3" // 导入SQLite3包但不使用，只用其驱动
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type Config struct {
	ListTarget         bool   `mapstructure:"list-target"`               // 一次性从目标S3获取列表进行对比再开始传输，文件数量大的情况可以节省每次请求之前逐个文件对比的API Call
	SkipCompare        bool   `mapstructure:"skip-compare"`              // 是否不做目标S3与源文件的对比，即无论是否有重复文件，都直接开始传输并覆盖
	TransferMetadata   bool   `mapstructure:"transfer-metadata"`         // 是否传输源S3 Object MetaData到目标S3，只在S3toS3模式下可用
	HttpTimeout        int    `mapstructure:"http-timeout"`              // S3 http 超时时间(秒)
	MaxRetries         int    `mapstructure:"max-retries"`               // API 请求最大重试次数
	ResumableThreshold int64  `mapstructure:"resumable-threshold"`       // 走断点续传流程的门槛，小于该值则直接并发下载，对于文件不大或不担心中断的情况效率更高（单位MB）
	NumWorkers         int    `mapstructure:"num-workers"`               // 控制 goroutine 总量
	WorkMode           string `mapstructure:"work-mode"`                 // SQS_SEND | SQS_CONSUME
	SQSUrl             string `mapstructure:"sqs-url"`                   // SQS Queue URL
	SQSProfile         string `mapstructure:"sqs-profile"`               // SQS Queue Profile
	YPtr               bool   `mapstructure:"y"`                         // Ignore waiting for confirming command
	DBPath             string `mapstructure:"db-location"`               // 自动创建已经下载的分片状态记录数据库
	ChunkSize          int64  `mapstructure:"chunk-size"`                // Multipart 分片大小
	RetryDelay         int    `mapstructure:"retry-delay"`               // API 请求重试延迟时间(秒)
	JobListPath        string `mapstructure:"joblist-write-to-filepath"` // 列出S3传输任务之后，写入到一个文件作为备份
	SQSSentLogName     string `mapstructure:"sqs-log-to-filename"`       // SQS已发送消息的记录文件名
	IgnoreListPath     string `mapstructure:"ignore-list-path"`          // List和传输的时候，如果S3源的Key或本地源路径的前缀在Ignore List里面，则跳过。设置的时候注意S3的Key是不带“/”开头的
	ForcePathStyle     bool   `mapstructure:"force-path-style"`          // 强制使用路径方式访问S3，而不是域名方式
}

type BInfo struct {
	url, bucket, prefix, profile, endpoint, region, storageClass, ACL string
	noSignRequest                                                     bool // The bucket is noSignRequest, no need to sign
	requestPayer                                                      bool // The bucket is requestPayer
	sess                                                              *session.Session
	svc                                                               *s3.S3
	downloader                                                        *s3manager.Downloader
	uploader                                                          *s3manager.Uploader
}

type MetaStruct struct {
	Metadata                                                                        map[string]*string
	ContentType, ContentLanguage, ContentEncoding, CacheControl, ContentDisposition *string
	Expires                                                                         *time.Time
}

type FileInfo struct {
	FromKey, FromBucket, ToKey, ToBucket string
	Size                                 int64
	File                                 *os.File
	Others                               MetaStruct
}

type PartInfo struct {
	FromKey, FromBucket, ToKey, ToBucket, Etag string
	Size, Offset                               int64
	PartNumber, TotalParts                     int64
}

type RetryFunc func() error

var (
	from, to               BInfo
	objectCount, sizeCount int64
	runningGoroutines      int32 // 当前正在运行的 goroutine 的数量
	cfg                    Config
	sqsSvc                 *sqs.SQS
)

var rootCmd = &cobra.Command{
	Use:   "s3trans FROM_URL TO_URL",
	Short: "s3trans transfers data from source to target",
	Long: `s3trans transfers data from source to target.
	./s3trans FROM_URL TO_URL [OPTIONS]
	FROM_URL: The url of data source, e.g. /home/user/data or s3://bucket/prefix
	TO_URL: The url of data transfer target, e.g. /home/user/data or s3://bucket/prefix
	For example:
	./s3trans s3://bucket/prefix s3://bucket/prefix -from_profile sin -to_profile bjs
	./s3trans s3://bucket/prefix /home/user/data -from_profile sin 
	`,
	Args: cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		// args[0] 是 FROM_URL, args[1] 是 TO_URL
		from.url = args[0]
		to.url = args[1]
	},
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().String("from-profile", "", "The AWS profile in ~/.aws/credentials of data source")
	viper.BindPFlag("from-profile", rootCmd.PersistentFlags().Lookup("from-profile"))
	rootCmd.PersistentFlags().Bool("force-path-style", false, "Set this to `true` to force the request to use path-style addressing See http://docs.aws.amazon.com/AmazonS3/latest/dev/VirtualHosting.html")
	viper.BindPFlag("force-path-style", rootCmd.PersistentFlags().Lookup("force-path-style"))
	rootCmd.PersistentFlags().String("to-profile", "", "The AWS profile in ~/.aws/credentials of data transfer target")
	viper.BindPFlag("to-profile", rootCmd.PersistentFlags().Lookup("to-profile"))
	rootCmd.PersistentFlags().String("from-endpoint", "", "The endpoint of data source, e.g. https://storage.googleapis.com; https://oss-<region>.aliyuncs.com; https://cos.<region>.myqcloud.com . If AWS s3 or local path, no need to specify this.")
	viper.BindPFlag("from-endpoint", rootCmd.PersistentFlags().Lookup("from-endpoint"))
	rootCmd.PersistentFlags().String("to-endpoint", "", "The endpoint of data transfer target, e.g. https://storage.googleapis.com . If AWS s3 or local path, no need to specify this.")
	viper.BindPFlag("to-endpoint", rootCmd.PersistentFlags().Lookup("to-endpoint"))
	rootCmd.PersistentFlags().String("from-region", "", "The region of data transfer source, e.g. cn-north-1. If no specified, the region will be auto detected with the credentials you provided in profile.")
	viper.BindPFlag("from-region", rootCmd.PersistentFlags().Lookup("from-region"))
	rootCmd.PersistentFlags().String("to-region", "", "The region of data transfer target, e.g. us-east-1. If no specified, the region will be auto detected with the credentials you provided in profile.")
	viper.BindPFlag("to-region", rootCmd.PersistentFlags().Lookup("to-region"))
	rootCmd.PersistentFlags().String("storage-class", "", "The TARGET S3 bucket storage class, e.g. STANDARD|REDUCED_REDUNDANCY|STANDARD_IA|ONEZONE_IA|INTELLIGENT_TIERING|GLACIER|DEEP_ARCHIVE|OUTPOSTS|GLACIER_IR|SNOW or others of S3 compatibale")
	viper.BindPFlag("storage-class", rootCmd.PersistentFlags().Lookup("storage-class"))
	rootCmd.PersistentFlags().String("acl", "", "The TARGET S3 bucket ACL, private means only the object owner can read&write, e.g. private|public-read|public-read-write|authenticated-read|aws-exec-read|bucket-owner-read|bucket-owner-full-control")
	viper.BindPFlag("acl", rootCmd.PersistentFlags().Lookup("acl"))
	rootCmd.PersistentFlags().Bool("no-sign-request", false, "The SOURCE bucket is not needed to sign the request")
	viper.BindPFlag("no-sign-request", rootCmd.PersistentFlags().Lookup("no-sign-request"))
	rootCmd.PersistentFlags().Bool("request-payer", false, "The SOURCE bucket requires requester to pay, set this")
	viper.BindPFlag("request-payer", rootCmd.PersistentFlags().Lookup("request-payer"))
	rootCmd.PersistentFlags().String("db-location", "./download-status.db", "local db to record download resumable status")
	viper.BindPFlag("db-location", rootCmd.PersistentFlags().Lookup("db-location"))

	rootCmd.PersistentFlags().BoolP("list-target", "l", false, "List the TARGET S3 bucket, compare exist objects BEFORE transfer. List is more efficient than head each object to check if it exists, but transfer may start slower because it needs to wait for listing all objects to compare. To mitigate this, this app leverage Concurrency Listing for fast list; If no list-target para, transfer without listing the target S3 bucket, but before transfering each object, head each target object to check, this costs more API call, but start faster.")
	viper.BindPFlag("list-target", rootCmd.PersistentFlags().Lookup("list-target"))
	rootCmd.PersistentFlags().BoolP("skip-compare", "s", false, "If True, skip to compare the name and size between source and target S3 object. Just overwrite all objects. No list target nor head target object to check if it already exists.")
	viper.BindPFlag("skip-compare", rootCmd.PersistentFlags().Lookup("skip-compare"))
	rootCmd.PersistentFlags().Bool("transfer-metadata", false, "If True, get metadata from source S3 bucket and upload the metadata to target object. This costs more API calls.")
	viper.BindPFlag("transfer-metadata", rootCmd.PersistentFlags().Lookup("transfer-metadata"))

	rootCmd.PersistentFlags().Int("http-timeout", 30, "API request timeout (seconds)")
	viper.BindPFlag("http-timeout", rootCmd.PersistentFlags().Lookup("http-timeout"))
	rootCmd.PersistentFlags().Int("max-retries", 5, "API request max retries")
	viper.BindPFlag("max-retries", rootCmd.PersistentFlags().Lookup("max-retries"))
	rootCmd.PersistentFlags().Int("retry-delay", 5, "Delay before next retry in secondes")
	viper.BindPFlag("retry-delay", rootCmd.PersistentFlags().Lookup("retry-delay"))
	rootCmd.PersistentFlags().Int64("chunk-size", 5, "Multipart part size(MB)")
	viper.BindPFlag("chunk-size", rootCmd.PersistentFlags().Lookup("chunk-size"))
	rootCmd.PersistentFlags().Int64("resumable-threshold", 50, "When the file size (MB) is larger than this value, the file will be resumable transfered.")
	viper.BindPFlag("resumable-threshold", rootCmd.PersistentFlags().Lookup("resumable-threshold"))
	rootCmd.PersistentFlags().IntP("num-workers", "n", 4, "Max concurrent threads = NumWorkers*NumWorkers*4 (files*parts*4), recommend NumWorkers <= vCPU number")
	viper.BindPFlag("num-workers", rootCmd.PersistentFlags().Lookup("num-workers"))
	rootCmd.PersistentFlags().BoolP("y", "y", false, "Ignore waiting for confirming command")
	viper.BindPFlag("y", rootCmd.PersistentFlags().Lookup("y"))

	rootCmd.PersistentFlags().String("work-mode", "", "SQS_SEND | SQS_CONSUME | DRYRUN; SQS_SEND means listing source FROM_URL S3 and target TO_URL S3 to compare and send message to SQS queue, SQS_CONSUME means consume message from SQS queue and transfer objects from FROM_URL S3 to TO_URL S3; DRYRUN means only count the objects and sizes comparing delta list of FROM_URL S3 and TO_URL S3, no transfer.")
	viper.BindPFlag("work-mode", rootCmd.PersistentFlags().Lookup("work-mode"))
	rootCmd.PersistentFlags().String("sqs-url", "", "The SQS queue URL to send or consume message from, e.g. https://sqs.us-east-1.amazonaws.com/my_account/my_queue_name")
	viper.BindPFlag("sqs-url", rootCmd.PersistentFlags().Lookup("sqs-url"))
	rootCmd.PersistentFlags().String("sqs-profile", "", "The SQS queue leverage which AWS profile in ~/.aws/credentials")
	viper.BindPFlag("sqs-profile", rootCmd.PersistentFlags().Lookup("sqs-profile"))
	rootCmd.PersistentFlags().String("joblist-write-to-filepath", "", "After listing source and target S3, compare the delta joblist and write the joblist to this filepath, e.g. ./joblist.txt")
	viper.BindPFlag("joblist-write-to-filepath", rootCmd.PersistentFlags().Lookup("joblist-write-to-filepath"))
	rootCmd.PersistentFlags().String("sqs-log-to-filename", "", "After sent joblist to SQS, write the sent messages log to this filepath, e.g. ./sqs-log.txt")
	viper.BindPFlag("sqs-log-to-filename", rootCmd.PersistentFlags().Lookup("sqs-log-to-filename"))
	rootCmd.PersistentFlags().String("ignore-list-path", "", "When listing and transfer, if source S3 key or local path matching the prefix in this ignore-list, it will be ignored. This is useful to ignore some objects that are not needed to transfer. The ignore-list is a file path, e.g. ./ignore-list.txt")
	viper.BindPFlag("ignore-list-path", rootCmd.PersistentFlags().Lookup("ignore-list-path"))
}

func initConfig() {
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	viper.SetConfigFile("config.yaml") // YAML 格式配置文件 config.yaml
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
	// Unmarshal config into cfg struct
	if err := viper.Unmarshal(&cfg); err != nil {
		fmt.Println("Error unmarshalling config:", err)
		os.Exit(1)
	}
}

func getConfig() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	from.profile = viper.GetString("from-profile")
	to.profile = viper.GetString("to-profile")
	from.endpoint = viper.GetString("from-endpoint")
	to.endpoint = viper.GetString("to-endpoint")
	from.region = viper.GetString("from-region")
	to.region = viper.GetString("to-region")
	to.storageClass = viper.GetString("storage-class")
	to.ACL = viper.GetString("acl")
	from.noSignRequest = viper.GetBool("no-sign-request")
	from.requestPayer = viper.GetBool("request-payer")
	cfg.ResumableThreshold = cfg.ResumableThreshold * 1024 * 1024
	cfg.ChunkSize = cfg.ChunkSize * 1024 * 1024

	for i, binfo := range []*BInfo{&from, &to} {
		if i == 0 {
			fmt.Print("From ")
		} else {
			fmt.Print("To ")
		}
		if strings.HasPrefix(binfo.url, "s3://") {
			// Parse S3 URL
			URL, err := url.Parse(binfo.url)
			if err != nil {
				log.Fatalf("Invalid S3 URL: %s, %v\n", binfo.url, err)
			}
			binfo.bucket = URL.Host
			binfo.prefix = strings.TrimSuffix(strings.TrimPrefix(URL.Path, "/"), "/")
			binfo.sess = getSess(binfo)
			binfo.svc = s3.New(binfo.sess)
			if i == 0 {
				binfo.downloader = s3manager.NewDownloader(binfo.sess)
				binfo.downloader.Concurrency = cfg.NumWorkers * 4
				binfo.downloader.PartSize = cfg.ChunkSize
			} else {
				binfo.uploader = s3manager.NewUploader(binfo.sess)
				binfo.uploader.Concurrency = cfg.NumWorkers * 4
				binfo.uploader.PartSize = cfg.ChunkSize
			}
			fmt.Printf("Bucket: %s, Prefix: %s, Profile: %s, Endpoint-URL: %s, Region:%s\n", binfo.bucket, binfo.prefix, binfo.profile, binfo.endpoint, binfo.region)
		} else

		// TODO: Azure Blog Storage

		{
			// Verify the local path
			urlInfo, err := os.Stat(binfo.url)
			if err != nil {
				log.Printf("Invalid path, try to create directories: %s\n", binfo.url) // 自动创建新目录
				if err := os.MkdirAll(binfo.url, 0755); err != nil {
					log.Fatalln("Failed to create directories:", binfo.url, err)
				}
			} else {
				if urlInfo.IsDir() && !strings.HasSuffix(binfo.url, string(os.PathSeparator)) {
					binfo.url += string(os.PathSeparator)
				}
				fmt.Printf("Local: %s\n", binfo.url)
			}
		}
	}
	if cfg.WorkMode == "SQS_SEND" || cfg.WorkMode == "SQS_CONSUME" {
		sqsSvc = getSQSsess()
	}

}

func main() {
	startTime := time.Now()
	getConfig()
	fmt.Printf(" Target StorageClass(default: STANDARD): %s\n Target ACL(default: private): %s\n Source noSignRequest: %t\n Source requestPayer: %t\n", to.storageClass, to.ACL, from.noSignRequest, from.requestPayer)
	fmt.Printf(" Transfer Metadata: %t\n List Target Before Transfer(Recommended): %t\n Skip Compare Before Transfer: %t\n", cfg.TransferMetadata, cfg.ListTarget, cfg.SkipCompare)
	fmt.Printf(" NumWorkers: %d for concurrency files; NumWorkers*4 for parts of each file and for listing target bucket\n", cfg.NumWorkers)
	fmt.Printf(" HttpTimeout: %ds\n MaxRetries: %d\n ResumableThreshold: %s\n", cfg.HttpTimeout, cfg.MaxRetries, ByteCountSI(cfg.ResumableThreshold))
	fmt.Printf(" ChunkSize: %s\n", ByteCountSI(cfg.ChunkSize))
	fmt.Printf(" WorkMode: %s\n SQS_PROFILE: %s\n SQS_URL: %s\n", cfg.WorkMode, cfg.SQSProfile, cfg.SQSUrl)
	fmt.Printf("Start to transfer data? (y/n): \n")
	if !cfg.YPtr {
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" {
			log.Fatalln("Exit app with n command.")
		}
	}
	switch {
	case cfg.WorkMode == "DRYRUN":
		err := compareBucket(from, to, nil)
		if err != nil {
			log.Println("Failed to count:", err)
			return
		}
	case cfg.WorkMode == "SQS_SEND":
		err := compareBucket(from, to, sqsSvc)
		if err != nil {
			log.Println("Failed to send sqs:", err)
			return
		}
	case cfg.WorkMode == "SQS_CONSUME":
		err := consumeSQS(sqsSvc)
		if err != nil {
			log.Println("Failed to consume sqs:", err)
			return
		}
	case strings.HasPrefix(from.url, "s3://") && strings.HasPrefix(to.url, "s3://"):
		cfg.WorkMode = "S3TOS3"
		err := s3tos3(from, to)
		if err != nil {
			log.Println("Failed to s3tos3:", err)
			return
		}
	case strings.HasPrefix(from.url, "s3://"):
		cfg.WorkMode = "GET"
		err := startDownload(from, to)
		if err != nil {
			log.Println("Failed to download:", err)
			return
		}
	case strings.HasPrefix(to.url, "s3://"):
		cfg.WorkMode = "PUT"
		err := startUpload(from, to)
		if err != nil {
			log.Println("Failed to upload:", err)
			return
		}
	default:
		log.Fatal("ERR WorkMode, invalid FROM_URL or TO_URL")
	}
	log.Printf("\n\nTotalObjects:%d, TotalSizes:%s(%d). The program ran for %v\n", objectCount, ByteCountSI(sizeCount), sizeCount, time.Since(startTime))
	log.Println("From:", from.url)
	log.Println("To:", to.url)
}

type RetryRoundTripper struct {
	Proxied http.RoundTripper
	Retries int
	Delay   time.Duration
}

func (rrt *RetryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i <= rrt.Retries; i++ {
		resp, err = rrt.Proxied.RoundTrip(req)
		if err != nil {
			log.Printf("HTTP API Request failed and retry: %s", err)
			time.Sleep(rrt.Delay)
			continue
		}
		break
	}
	return resp, err
}
func getSess(bInfo *BInfo) *session.Session {
	// 创建具有超时重试的 http 客户端
	client := &http.Client{
		Timeout: time.Duration(cfg.HttpTimeout) * time.Second,
		Transport: &RetryRoundTripper{
			Proxied: http.DefaultTransport,
			Retries: cfg.MaxRetries,
			Delay:   time.Duration(cfg.RetryDelay) * time.Second,
		},
	}
	config := aws.Config{
		MaxRetries:       aws.Int(cfg.MaxRetries), // 自定义S3 Client最大重试次数
		HTTPClient:       client,                  // 使用自定义了超时时间的 http 客户端
	}
	if cfg.ForcePathStyle {
		config.S3ForcePathStyle = aws.Bool(true) // 以路径方式访问 而不是域名
	}
	if bInfo.endpoint != "" {
		completeEndpointURL(bInfo) // 自动完善endpoint url
		config.Endpoint = aws.String(bInfo.endpoint)
	}
	// 如果noSignRequest 则必须要有region
	if bInfo.noSignRequest {
		if bInfo.region != "" {
			config.Credentials = credentials.AnonymousCredentials
		} else {
			log.Fatalf("No region specified for noSignRequest bucket: %s\n", bInfo.bucket)
		}
	} else if bInfo.region == "" {
		// Call GetBucketLocation to determine the bucket's region.
		tempS3sess, err := session.NewSessionWithOptions(session.Options{
			Config:            config,
			Profile:           bInfo.profile, // ~/.aws/目录下，文件名为config或者credentials
			SharedConfigState: session.SharedConfigEnable,
		})
		if err != nil {
			log.Fatalf("Failed to create session with reading ~/.aws/credentials profile: %s, with endpoint: %s err: %v\n", bInfo.profile, bInfo.endpoint, err)
		}
		result, err := s3.New(tempS3sess).GetBucketLocation(&s3.GetBucketLocationInput{
			Bucket: aws.String(bInfo.bucket),
		})
		if err != nil {
			log.Fatalf("Failed to get bucket location: %s, err: %v\n", bInfo.bucket, err)
		}
		if result.LocationConstraint == nil {
			bInfo.region = "us-east-1" // Default bucket's region is us-east-1
		} else {
			bInfo.region = aws.StringValue(result.LocationConstraint)
		}
	}
	config.Region = aws.String(bInfo.region)
	sess, err := session.NewSessionWithOptions(session.Options{
		Config:            config,
		Profile:           bInfo.profile,
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		log.Fatalf("Failed to create session with reading ~/.aws/credentials profile: %s, in bucket region: %s, with endpoint: %s err: %v\n", bInfo.profile, bInfo.region, bInfo.endpoint, err)
	}
	return sess
}

// 自动完善endpoint url
func completeEndpointURL(bInfo *BInfo) {
	switch bInfo.endpoint {
	case "Aliyun_OSS":
		if bInfo.region == "" {
			log.Fatalf("No region specified for bucket: %s\n", bInfo.bucket)
		}
		bInfo.endpoint = fmt.Sprintf("https://oss-%s.aliyuncs.com", bInfo.region)
	case "Tencent_COS":
		if bInfo.region == "" {
			log.Fatalf("No region specified for bucket:%s\n", bInfo.bucket)
		}
		bInfo.endpoint = fmt.Sprintf("https://cos.%s.myqcloud.com", bInfo.region)
	case "Google_GCS":
		bInfo.endpoint = "https://storage.googleapis.com"
	}
	// 都不是以上定义字符串则自直接使用endpoint url的字符串

}

func getSQSsess() *sqs.SQS {
	// get region from cfg.SQSUrl "https://sqs.us-east-1.amazonaws.com/my_account/my_queue_name"
	u, err := url.Parse(cfg.SQSUrl)
	if err != nil {
		log.Fatalln("fail to parse SQS url", err)
	}
	hostParts := strings.Split(u.Host, ".")
	if len(hostParts) < 2 {
		log.Fatalln("Invalid SQS URL")
	}
	SQSRegion := hostParts[1]

	// 创建具有超时的 http 客户端
	client := &http.Client{Timeout: time.Duration(cfg.HttpTimeout) * time.Second}
	config := aws.Config{
		MaxRetries: aws.Int(cfg.MaxRetries), // 自定义S3 Client最大重试次数
		HTTPClient: client,                  // 使用自定义了超时时间的 http 客户端
		Region:     aws.String(SQSRegion),
	}
	sqssess, err := session.NewSessionWithOptions(session.Options{
		Config:            config,
		Profile:           cfg.SQSProfile,
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		log.Fatalf("Failed to create SQS session with reading ~/.aws/credentials profile: %s, err: %v\n", from.profile, err)
	}
	sqsSvc := sqs.New(sqssess)
	return sqsSvc
}
