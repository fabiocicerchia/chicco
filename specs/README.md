# Specifications of chicco

What this code does, one atomic Gherkin scenario per behaviour, read from the code by code-atlas.
Tags: `@verified` a test asserts it · `@inferred` read from the code only · `@gap` an entry point no test covers · `@human` written or amended by a person, never rewritten.

## API Gateway
The OpenAI- and Anthropic-compatible endpoints that callers talk to, and how requests and responses keep their wire format.

- **Access control**: [Guarding the proxy with its own API key](api-gateway/access-control/proxy-access-key.feature) (6)
- **Anthropic messages**: [Answering non-streaming Anthropic messages](api-gateway/anthropic-messages/anthropic-message-replies.feature) (4), [Accepting Anthropic Messages requests for any provider](api-gateway/anthropic-messages/anthropic-request-translation.feature) (7)
- **Chat completions**: [Forwarding OpenAI chat and embeddings requests](api-gateway/chat-completions/openai-forwarding.feature) (2)
- **Model catalog**: [Listing only virtual models that can answer](api-gateway/model-catalog/model-catalog-servable-models.feature) (2)
- **Streaming responses**: [Streaming Anthropic messages](api-gateway/streaming/anthropic-message-streaming.feature) (7), [Translating provider replies into Anthropic messages](api-gateway/streaming/anthropic-reply-translation.feature) (2)

## Local CLI Providers
Using locally installed AI command-line tools (claude, codex, kiro and similar) as providers in the pool.

- **CLI error handling**: [Reporting why a CLI provider failed](cli-providers/cli-errors/cli-failure-reporting.feature) (6)
- **CLI execution**: [Serving requests from a local CLI tool](cli-providers/cli-execution/running-cli-providers.feature) (4)
- **Response synthesis**: [Answering callers from a CLI tool's output](cli-providers/cli-response-synthesis/cli-provider-replies.feature) (3), [Answering in OpenAI shape from a local CLI tool](cli-providers/cli-response-synthesis/cli-reply-synthesis.feature) (4)

## Configuration
Defining providers, models, prices and alerts, and changing them without a restart.

- **Config file**: [Describing providers and settings in chicco.yaml](configuration/config-file/config-file-shape.feature) (5), [Loading chicco.yaml](configuration/config-file/loading-config-file.feature) (7)
- **Config validation**: [Checking a config before it ships](configuration/config-validation/config-check.feature) (6)
- **Hot reload**: [Reloading the config without a restart](configuration/hot-reload/config-hot-reload.feature) (4), [Applying config changes on Windows](configuration/hot-reload/config-reload-windows.feature) (1), [Reloading the config with a hangup signal](configuration/hot-reload/reload-on-sighup.feature) (2)

## Dashboard
The live views operators use to watch and drive the proxy.

- **Live logs**: [Showing recent log lines](dashboard/live-logs/live-log-history.feature) (3)
- **Provider view**: [Seeing each provider's state and quota use in the terminal dashboard](dashboard/provider-view/terminal-provider-rows.feature) (5)
- **Terminal dashboard**: [Driving the terminal dashboard](dashboard/terminal-dashboard/terminal-dashboard-controls.feature) (1), [Reading the terminal dashboard](dashboard/terminal-dashboard/terminal-dashboard-display.feature) (4)
- **Web dashboard**: [Opening the web dashboard](dashboard/web-dashboard/web-dashboard-page.feature) (2)

## Operations
Starting, building, packaging and deploying chicco.

- **Container and Kubernetes**: [Running chicco from its container image](operations/container-and-kubernetes/chicco-container-image.feature) (4)
- **Installation**: [Installing chicco with the install script](operations/installation/install-script.feature) (7)
- **Startup and command line**: [Running and checking chicco from the command line](operations/startup/chicco-command-line.feature) (5), [Starting the proxy](operations/startup/starting-the-proxy.feature) (8)

## Provider Health
Knowing which providers are alive and which models actually work.

- **Health and status endpoints**: [Reporting proxy health and live status](provider-health/health-endpoint/proxy-health-and-status.feature) (2)
- **Health probes**: [Probing provider health](provider-health/health-probes/provider-health-probes.feature) (2)
- **Model testing**: [Testing every configured model on demand](provider-health/model-testing/model-testing.feature) (7)

## Routing and Failover
Choosing which provider serves each request and moving on transparently when one fails.

- **Cooldowns and quotas**: [Respecting configured rate limits before calling providers](routing/cooldowns-quotas/client-side-rate-limits-route.feature) (3), [Respecting provider rate limits](routing/cooldowns-quotas/client-side-rate-limits.feature) (6)
- **Error classification**: [Benching a provider after an upstream failure](routing/error-classification/upstream-failure-classification.feature) (5)
- **Provider rotation**: [Choosing which providers can take traffic](routing/provider-rotation/provider-pool-eligibility.feature) (1), [Routing requests through virtual models](routing/provider-rotation/virtual-model-routing.feature) (5)

## Usage and Cost
Measuring what the proxy consumes and warning before free tiers run out.

- **Quota alerts**: [Warning before a provider's quota runs out](usage-and-cost/quota-alerts/budget-alerts.feature) (3)
- **Spend tracking**: [Pricing requests and reporting session spend](usage-and-cost/spend-tracking/session-spend.feature) (1)
- **Usage persistence**: [Keeping usage and cooldowns across restarts](usage-and-cost/usage-persistence/usage-survives-restart.feature) (7)
