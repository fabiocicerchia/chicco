@domain:operations @area:container-and-kubernetes @from:chicco/dockerfile
Feature: Running chicco from its container image
  Operators who deploy chicco in Docker get an image that holds no secrets, runs unprivileged, finds its config and state at fixed paths and reports its own health.

  Background:
    Given the operator has the chicco container image

  @id:operations.container-and-kubernetes.001 @rule:operations.container.config-and-keys-mounted-not-baked @inferred @source:Dockerfile#CMD
  Scenario: The image holds no config or provider keys and reads them from the conventional mount points
    Given the operator's chicco.yaml holds provider API keys
    And the operator mounts chicco.yaml at /etc/chicco/chicco.yaml
    When the operator starts the container without extra arguments
    Then chicco loads its config from /etc/chicco/chicco.yaml
    And chicco keeps its usage state in /var/lib/chicco/chicco-state.json
    And the image itself has no chicco.yaml and no provider keys in it

  @id:operations.container-and-kubernetes.002 @rule:operations.container.args-replace-default-flags @inferred @source:Dockerfile#CMD
  Scenario: Flags given to the container replace the default config and state paths
    Given the operator mounts chicco.yaml at /config/chicco.yaml
    When the operator starts the container passing "-config /config/chicco.yaml" after the image name
    Then chicco loads its config from /config/chicco.yaml
    And the default -state path is no longer passed to chicco

  @id:operations.container-and-kubernetes.003 @rule:operations.container.runs-unprivileged @inferred @source:Dockerfile#USER
  Scenario: chicco runs as an unprivileged user inside the container
    When the operator starts the container
    Then the chicco process runs as the non-root user "chicco"
    And the chicco user can write to /var/lib/chicco

  @id:operations.container-and-kubernetes.004 @rule:operations.container.health-from-health-endpoint @inferred @source:Dockerfile#HEALTHCHECK @questionable
  Scenario Outline: The container reports healthy only while chicco answers its health endpoint on port 41986
    Given chicco.yaml sets an api_key and chicco is configured to listen on <addr>
    When the container runtime runs its health check
    Then the container is reported <state>

    Examples:
      | addr            | state     |
      | 0.0.0.0:41986   | healthy   |
      | 127.0.0.1:41986 | healthy   |
      | 0.0.0.0:8080    | unhealthy |
