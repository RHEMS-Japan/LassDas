package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// runtimeSecret is the JSON the hook reads from the secret vault; the field
// names are the engine's contract (internal/app/config.go).
type runtimeSecret struct {
	HookBasicUsername string `json:"hook_basic_username"`
	HookBasicPassword string `json:"hook_basic_password"`
	BacklogAPIKey     string `json:"backlog_api_key"`
	PullHMACKey       string `json:"pull_hmac_key"`
}

type cloudClients struct {
	dynamo  *dynamodb.Client
	secrets *secretsmanager.Client
	iam     *iam.Client
	lambda  *lambda.Client
}

func newCloudClients(a *Answers) (*cloudClients, error) {
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(a.Region)}
	if a.Profile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(a.Profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), options...)
	if err != nil {
		return nil, errors.New("AWS の資格情報が読めません (profile/環境変数を確認)")
	}
	return &cloudClients{
		dynamo:  dynamodb.NewFromConfig(cfg),
		secrets: secretsmanager.NewFromConfig(cfg),
		iam:     iam.NewFromConfig(cfg),
		lambda:  lambda.NewFromConfig(cfg),
	}, nil
}

// provisionCloud builds the receiving side: the state table, the runtime
// secret (the single source of truth for the HMAC key and the webhook's
// Basic credentials), the hook Lambda with its URL, and finally the four
// callback URL variables on the instance repository.
func provisionCloud(state *State) error {
	a := &state.Answers
	o := &state.Outputs
	ctx := context.Background()

	clients, err := newCloudClients(a)
	if err != nil {
		return err
	}

	tableName := a.NamePrefix + "-state"
	if err := ensureTable(ctx, clients.dynamo, tableName); err != nil {
		return err
	}
	o.TableName = tableName
	stepOK("台帳 (DynamoDB): " + tableName)

	secretName := a.NamePrefix + "/runtime"
	secretARN, secret, err := ensureRuntimeSecret(ctx, clients.secrets, secretName, a.BotAPIKey)
	if err != nil {
		return err
	}
	o.SecretARN = secretARN
	stepOK("秘密保管 (Secrets Manager): " + secretName)

	// The workflow signs its claims with the same key the hook verifies;
	// the vault is the source and the repository secret is the copy, so a
	// resumed session converges instead of forking the key.
	converge := map[string]string{"TICKET_INGRESS_PULL_HMAC_KEY": secret.PullHMACKey}
	if a.BotAPIKey != "" {
		// The vault and the repository copy of the tracker key move together,
		// so a rotation entered on resume cannot leave the worker on the old
		// key while the hook uses the new one.
		converge["BACKLOG_API_KEY"] = secret.BacklogAPIKey
	}
	for name, value := range converge {
		command := exec.Command("gh", "secret", "set", name, "-R", a.InstanceRepo)
		command.Stdin = strings.NewReader(value)
		if out, err := command.CombinedOutput(); err != nil {
			return errors.New(name + " の投入に失敗: " + strings.TrimSpace(string(out)))
		}
	}

	roleARN, err := ensureRole(ctx, clients.iam, a.NamePrefix+"-hook-role", tableName, secretARN, a.Region)
	if err != nil {
		return err
	}

	functionName := a.NamePrefix + "-hook"
	functionURL, err := ensureLambda(ctx, clients.lambda, functionName, roleARN, lambdaEnvironment(a, o, secretARN, tableName))
	if err != nil {
		return err
	}
	o.FunctionURL = functionURL
	stepOK("受け口 (Lambda + URL): " + functionURL)

	urls := map[string]string{
		"TICKET_INGRESS_CLAIM_URL":           functionURL + "pull-claim/v1",
		"TICKET_INGRESS_TICK_URL":            functionURL + "question-tick/v1",
		"TICKET_INGRESS_REPORT_URL":          functionURL + "terminal-report/v1",
		"TICKET_INGRESS_QUESTION_REPORT_URL": functionURL + "question-report/v1",
	}
	for name, value := range urls {
		if out, err := gh("variable", "set", name, "-R", a.InstanceRepo, "--body", value); err != nil {
			return errors.New("variable " + name + " の投入に失敗: " + out)
		}
	}
	stepOK("受け口の URL 4 本を repo vars に投入")
	return nil
}

func ensureTable(ctx context.Context, client *dynamodb.Client, name string) error {
	if _, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)}); err == nil {
		return nil
	}
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		return errors.New("台帳の作成に失敗: " + err.Error())
	}
	waiter := dynamodb.NewTableExistsWaiter(client)
	return waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)}, 2*time.Minute)
}

func ensureRuntimeSecret(ctx context.Context, client *secretsmanager.Client, name, backlogKey string) (string, runtimeSecret, error) {
	var secret runtimeSecret
	described, err := client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if err == nil {
		value, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
		if err != nil || value.SecretString == nil {
			return "", secret, errors.New("既存の秘密が読めません: " + name)
		}
		if err := json.Unmarshal([]byte(*value.SecretString), &secret); err != nil {
			return "", secret, errors.New("既存の秘密の形式が想定と違います: " + name)
		}
		// The interview may carry a rotated tracker key; the vault follows
		// the operator's latest answer.
		if backlogKey != "" && secret.BacklogAPIKey != backlogKey {
			secret.BacklogAPIKey = backlogKey
			if err := putSecret(ctx, client, name, secret); err != nil {
				return "", secret, err
			}
		}
		return *described.ARN, secret, nil
	}

	hmacKey, err := randomBase64(48)
	if err != nil {
		return "", secret, err
	}
	secret = runtimeSecret{
		HookBasicUsername: "hook",
		HookBasicPassword: randomHex(18),
		BacklogAPIKey:     backlogKey,
		PullHMACKey:       hmacKey,
	}
	encoded, _ := json.Marshal(secret)
	created, err := client.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(string(encoded)),
	})
	if err != nil {
		return "", secret, errors.New("秘密の作成に失敗: " + err.Error())
	}
	return *created.ARN, secret, nil
}

func putSecret(ctx context.Context, client *secretsmanager.Client, name string, secret runtimeSecret) error {
	encoded, _ := json.Marshal(secret)
	_, err := client.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     aws.String(name),
		SecretString: aws.String(string(encoded)),
	})
	if err != nil {
		return errors.New("秘密の更新に失敗: " + err.Error())
	}
	return nil
}

func ensureRole(ctx context.Context, client *iam.Client, name, tableName, secretARN, region string) (string, error) {
	trust := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	var roleARN string
	if got, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)}); err == nil {
		roleARN = *got.Role.Arn
	} else {
		created, err := client.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(name),
			AssumeRolePolicyDocument: aws.String(trust),
		})
		if err != nil {
			return "", errors.New("IAM role の作成に失敗: " + err.Error())
		}
		roleARN = *created.Role.Arn
	}
	// The exact set the engine calls, counted from internal/state: GetItem,
	// and transactional Put/Update/Delete/ConditionCheck entries - a
	// transaction authorizes per entry, and "dynamodb:TransactWriteItems" is
	// not an IAM action at all. Delete looks droppable but is the serial
	// slot's release; without it every completed ticket blocks the next one
	// forever, and the wizard's own smoke never touches the table, so the
	// gap only surfaces on the second live ticket.
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
  {"Effect":"Allow","Action":["dynamodb:GetItem","dynamodb:PutItem","dynamodb:UpdateItem","dynamodb:DeleteItem","dynamodb:ConditionCheckItem"],"Resource":"arn:aws:dynamodb:%s:*:table/%s"},
  {"Effect":"Allow","Action":"secretsmanager:GetSecretValue","Resource":"%s"},
  {"Effect":"Allow","Action":["logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
		region, tableName, secretARN)
	_, err := client.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(name),
		PolicyName:     aws.String(name + "-inline"),
		PolicyDocument: aws.String(policy),
	})
	if err != nil {
		return "", errors.New("IAM policy の投入に失敗: " + err.Error())
	}
	return roleARN, nil
}

// packageLambda builds the hook archive with the same script CI uses, so the
// deployed binary is the reviewed build path and not a wizard special.
func packageLambda() (string, error) {
	output, err := os.MkdirTemp("", "lassdas-lambda-*")
	if err != nil {
		return "", err
	}
	command := exec.Command("sh", "scripts/package-lambda.sh", output)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return "", errors.New("受け口のビルドに失敗しました (scripts/package-lambda.sh)")
	}
	return filepath.Join(output, "ticket-ingress-lambda-arm64.zip"), nil
}

func ensureLambda(ctx context.Context, client *lambda.Client, name, roleARN string, environment map[string]string) (string, error) {
	archive, err := packageLambda()
	if err != nil {
		return "", err
	}
	code, err := os.ReadFile(archive)
	if err != nil {
		return "", err
	}

	_, getErr := client.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String(name)})
	if getErr != nil {
		_, err = client.CreateFunction(ctx, &lambda.CreateFunctionInput{
			FunctionName:  aws.String(name),
			Role:          aws.String(roleARN),
			Runtime:       lambdatypes.RuntimeProvidedal2023,
			Architectures: []lambdatypes.Architecture{lambdatypes.ArchitectureArm64},
			Handler:       aws.String("bootstrap"),
			Timeout:       aws.Int32(30),
			MemorySize:    aws.Int32(256),
			Code:          &lambdatypes.FunctionCode{ZipFile: code},
			Environment:   &lambdatypes.Environment{Variables: environment},
		})
		if err != nil {
			// A brand-new role measurably needs a few seconds before Lambda
			// accepts it; one bounded retry covers the propagation.
			if strings.Contains(err.Error(), "assume") || strings.Contains(err.Error(), "InvalidParameterValue") {
				time.Sleep(10 * time.Second)
				_, err = client.CreateFunction(ctx, &lambda.CreateFunctionInput{
					FunctionName:  aws.String(name),
					Role:          aws.String(roleARN),
					Runtime:       lambdatypes.RuntimeProvidedal2023,
					Architectures: []lambdatypes.Architecture{lambdatypes.ArchitectureArm64},
					Handler:       aws.String("bootstrap"),
					Timeout:       aws.Int32(30),
					MemorySize:    aws.Int32(256),
					Code:          &lambdatypes.FunctionCode{ZipFile: code},
					Environment:   &lambdatypes.Environment{Variables: environment},
				})
			}
			if err != nil {
				return "", errors.New("受け口の作成に失敗: " + err.Error())
			}
		}
	} else {
		if _, err := client.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{
			FunctionName: aws.String(name), ZipFile: code,
		}); err != nil {
			return "", errors.New("受け口の更新に失敗: " + err.Error())
		}
		if err := waitLambdaReady(ctx, client, name); err != nil {
			return "", err
		}
		if _, err := client.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
			FunctionName: aws.String(name),
			Environment:  &lambdatypes.Environment{Variables: environment},
		}); err != nil {
			return "", errors.New("受け口の設定更新に失敗: " + err.Error())
		}
	}
	if err := waitLambdaReady(ctx, client, name); err != nil {
		return "", err
	}

	functionURL := ""
	if got, err := client.GetFunctionUrlConfig(ctx, &lambda.GetFunctionUrlConfigInput{FunctionName: aws.String(name)}); err == nil {
		functionURL = *got.FunctionUrl
	} else {
		created, err := client.CreateFunctionUrlConfig(ctx, &lambda.CreateFunctionUrlConfigInput{
			FunctionName: aws.String(name),
			AuthType:     lambdatypes.FunctionUrlAuthTypeNone,
		})
		if err != nil {
			return "", errors.New("受け口 URL の作成に失敗: " + err.Error())
		}
		functionURL = *created.FunctionUrl
	}
	// Re-asserted on every run: a URL surviving from an interrupted session
	// without its invoke permission would otherwise stay broken forever.
	if _, err := client.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName:        aws.String(name),
		StatementId:         aws.String("public-url-invoke"),
		Action:              aws.String("lambda:InvokeFunctionUrl"),
		Principal:           aws.String("*"),
		FunctionUrlAuthType: lambdatypes.FunctionUrlAuthTypeNone,
	}); err != nil && !strings.Contains(err.Error(), "ResourceConflict") {
		return "", errors.New("受け口 URL の公開許可に失敗: " + err.Error())
	}
	return functionURL, nil
}

func waitLambdaReady(ctx context.Context, client *lambda.Client, name string) error {
	for attempt := 0; attempt < 30; attempt++ {
		got, err := client.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String(name)})
		if err == nil && got.State == lambdatypes.StateActive && got.LastUpdateStatus != lambdatypes.LastUpdateStatusInProgress {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("受け口が Active になりません")
}

// lambdaEnvironment is the hook's full contract. The tracker stage fills the
// category and board IDs afterwards, once they exist.
func lambdaEnvironment(a *Answers, o *Outputs, secretARN, tableName string) map[string]string {
	workflowRef := a.InstanceRepo + "/.github/workflows/receive-backlog-ticket.yml@refs/heads/main"
	destinations, _ := json.Marshal([]map[string]string{{
		"repository":        a.ConsumerRepo,
		"delivery":          "pull_request",
		"staging_origin":    a.StagingOrigin,
		"production_origin": a.ProductionOrigin,
	}})
	return map[string]string{
		"QUEUE_TABLE_NAME":           tableName,
		"RUNTIME_SECRET_ID":          secretARN,
		"AUTOMATION_RUN_ID":          o.AutomationRunID,
		"BACKLOG_ORIGIN":             "https://" + a.BacklogDomain,
		"BACKLOG_SPACE_KEY":          spaceKey(a.BacklogDomain),
		"BACKLOG_PROJECT_ID":         fmt.Sprintf("%d", a.ResolvedProjectID),
		"BACKLOG_PROJECT_KEY":        a.ProjectKey,
		"BACKLOG_ALLOWED_CREATOR_ID": a.AllowedCreatorID,
		"PULL_REPOSITORY_ID":         fmt.Sprintf("%d", o.InstanceRepoID),
		"PULL_REPOSITORY_SHA256":     sha256Hex(a.InstanceRepo),
		"PULL_WORKFLOW_REF_SHA256":   sha256Hex(workflowRef),
		"REPORT_DESTINATIONS":        string(destinations),
	}
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// updateLambdaEnvironment merges additional variables into the hook without
// disturbing the ones already set; the tracker stage uses it for the IDs it
// creates.
func updateLambdaEnvironment(a *Answers, extra map[string]string) error {
	ctx := context.Background()
	clients, err := newCloudClients(a)
	if err != nil {
		return err
	}
	name := a.NamePrefix + "-hook"
	current, err := clients.lambda.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String(name)})
	if err != nil {
		return errors.New("受け口の現設定が読めません: " + err.Error())
	}
	merged := map[string]string{}
	if current.Environment != nil {
		for key, value := range current.Environment.Variables {
			merged[key] = value
		}
	}
	for key, value := range extra {
		merged[key] = value
	}
	if err := waitLambdaReady(ctx, clients.lambda, name); err != nil {
		return err
	}
	if _, err := clients.lambda.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String(name),
		Environment:  &lambdatypes.Environment{Variables: merged},
	}); err != nil {
		return errors.New("受け口の設定更新に失敗: " + err.Error())
	}
	return waitLambdaReady(ctx, clients.lambda, name)
}

// readRuntimeSecret hands the tracker stage the Basic credentials for the
// webhook URL, straight from the vault.
func readRuntimeSecret(a *Answers, secretARN string) (runtimeSecret, error) {
	var secret runtimeSecret
	clients, err := newCloudClients(a)
	if err != nil {
		return secret, err
	}
	value, err := clients.secrets.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{SecretId: aws.String(secretARN)})
	if err != nil || value.SecretString == nil {
		return secret, errors.New("秘密が読めません")
	}
	if err := json.Unmarshal([]byte(*value.SecretString), &secret); err != nil {
		return secret, errors.New("秘密の形式が想定と違います")
	}
	return secret, nil
}

// checkCloudCredentials proves the answered profile can actually sign a
// request before anything is built, so a credential problem surfaces as the
// first message and not after the GitHub stage.
func checkCloudCredentials(a *Answers) error {
	ctx := context.Background()
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(a.Region)}
	if a.Profile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(a.Profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return errors.New("AWS の資格情報が読めません (profile/環境変数を確認)")
	}
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		return errors.New("AWS の資格情報で署名できません (" + a.Region + " / profile=" + a.Profile + ") — 認証を確認して再実行してください")
	}
	stepOK("AWS 資格情報を確認")
	return nil
}
