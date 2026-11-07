@domain:routing @area:provider-rotation @from:proxy/rotator
Feature: Choosing which providers can take traffic
  Lets operators trust that chicco only routes callers to providers that are configured well enough to answer, and that a model alias reaches the model it names.

  Background:
    Given chicco is running with a provider pool loaded from chicco.yaml

  @id:routing.provider-rotation.001 @rule:routing.provider-pool.only-configured-providers-routable @verified @source:internal/proxy/rotator.go#Provider.isActive @test:internal/proxy/proxy_test.go#TestActiveSkipsUnconfigured
  Scenario Outline: Only providers with models and credentials receive requests
    Given a provider "<provider>" of kind <kind> with api_key <api_key> and models <models>
    When a caller sends a chat completion request for chicco:auto
    Then the provider "<provider>" <outcome> as a candidate to serve the request

    Examples:
      | provider   | kind | api_key       | models   | outcome          |
      | nokey      | http | ""            | [m]      | is never offered |
      | blankkey   | http | "   "         | [m]      | is never offered |
      | nomodel    | http | "sk-test-123" | []       | is never offered |
      | ok         | http | "sk-test-123" | [m]      | is offered       |
      | claude-cli | cli  | ""            | [sonnet] | is offered       |
      | empty-cli  | cli  | ""            | []       | is never offered |
