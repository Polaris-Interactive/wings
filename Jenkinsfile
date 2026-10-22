// Jenkins equivalent of the GitHub Actions workflows in .github/workflows
// (lint.yaml, push.yaml, docker.yaml and release.yaml).
//
// Use it from a Multibranch Pipeline job, with tag discovery enabled so that
// "v*" tags are built as releases. The agent needs Docker (the Go toolchains
// run in the official golang images) and the Docker Pipeline plugin.
//
// Before the first run, set DOCKER_REGISTRY and DOCKER_IMAGE below and add a
// "Username with password" credential for the registry under the id in
// DOCKER_CREDENTIALS.

pipeline {
    agent none

    options {
        buildDiscarder(logRotator(numToKeepStr: '30', artifactNumToKeepStr: '10'))
        timeout(time: 1, unit: 'HOURS')
        timestamps()
    }

    triggers {
        // Check the repository for changes every few minutes. This can be
        // removed if the git server sends webhooks to Jenkins.
        pollSCM('H/5 * * * *')
    }

    environment {
        SRC_PATH = 'github.com/pterodactyl/wings'

        DOCKER_REGISTRY = 'docker.astralrealms.fr'
        DOCKER_IMAGE = 'wings'
        DOCKER_CREDENTIALS = credentials('docker-registry')
    }

    stages {
        stage('Lint') {
            agent {
                docker {
                    image 'golangci/golangci-lint:v2.11.4'
                }
            }
            environment {
                HOME = "${WORKSPACE}/.home"
                GOPATH = "${WORKSPACE}/.go"
                GOCACHE = "${WORKSPACE}/.cache/go-build"
                GOLANGCI_LINT_CACHE = "${WORKSPACE}/.cache/golangci-lint"
            }
            steps {
                // Wings only targets Linux, so lint as Linux to cover the _linux/_unix files.
                sh 'GOOS=linux golangci-lint run ./...'
            }
        }

        stage('Build and Test') {
            matrix {
                axes {
                    axis {
                        name 'GO_VERSION'
                        values '1.26.9', '1.27.2'
                    }
                    axis {
                        name 'GOARCH'
                        values 'amd64', 'arm64'
                    }
                }
                agent {
                    docker {
                        image "golang:${GO_VERSION}"
                    }
                }
                environment {
                    HOME = "${WORKSPACE}/.home"
                    GOPATH = "${WORKSPACE}/.go"
                    GOCACHE = "${WORKSPACE}/.cache/go-build"
                    GOOS = 'linux'
                }
                stages {
                    stage('Build') {
                        steps {
                            sh '''
                                set -eu
                                export CGO_ENABLED=0
                                go mod download
                                VERSION="dev-$(echo "$GIT_COMMIT" | cut -c1-7)"
                                rm -rf dist
                                go build -v -trimpath -ldflags="-s -w -X ${SRC_PATH}/system.Version=${VERSION}" -o "dist/wings_linux_${GOARCH}" ${SRC_PATH}
                                go build -v -trimpath -ldflags="-X ${SRC_PATH}/system.Version=${VERSION}" -o "dist/wings_linux_${GOARCH}_debug" ${SRC_PATH}
                                chmod 755 dist/*
                            '''
                        }
                    }
                    stage('Test') {
                        when {
                            environment name: 'GOARCH', value: 'amd64'
                        }
                        steps {
                            // Tests run for the host architecture.
                            sh 'GOARCH= CGO_ENABLED=0 go test $(go list ./...)'
                            sh 'GOARCH= CGO_ENABLED=1 go test -race $(go list ./...)'
                        }
                    }
                    stage('Archive') {
                        when {
                            allOf {
                                environment name: 'GO_VERSION', value: '1.27.2'
                                anyOf {
                                    branch 'develop'
                                    changeRequest()
                                }
                            }
                        }
                        steps {
                            archiveArtifacts artifacts: 'dist/*', fingerprint: true
                        }
                    }
                }
            }
        }

        stage('Docker') {
            agent any
            when {
                anyOf {
                    tag pattern: 'v*', comparator: 'GLOB'
                    allOf {
                        branch 'develop'
                        // A tag is always built, but a commit to develop can opt out
                        // with "skip docker" or "docker skip" in its message.
                        expression {
                            def message = sh(script: 'git log -1 --pretty=%B', returnStdout: true).toLowerCase()
                            return !message.contains('skip docker') && !message.contains('docker skip')
                        }
                    }
                }
            }
            steps {
                withCredentials([usernamePassword(credentialsId: env.DOCKER_CREDENTIALS, usernameVariable: 'REGISTRY_USER', passwordVariable: 'REGISTRY_PASSWORD')]) {
                    sh '''
                        set -eu
                        echo "$REGISTRY_PASSWORD" | docker login "$DOCKER_REGISTRY" -u "$REGISTRY_USER" --password-stdin

                        # Multi-platform builds need QEMU for arm64 and a buildx builder
                        # using the docker-container driver.
                        docker run --privileged --rm tonistiigi/binfmt --install arm64
                        docker buildx inspect wings-builder >/dev/null 2>&1 || docker buildx create --name wings-builder --driver docker-container
                        docker buildx use wings-builder

                        if [ -n "${TAG_NAME:-}" ]; then
                            VERSION="${TAG_NAME#v}"
                            TAGS="--tag ${DOCKER_IMAGE}:${TAG_NAME}"
                            # Pre-releases are never tagged as latest.
                            case "$TAG_NAME" in
                                *rc*|*beta*|*alpha*) ;;
                                *) TAGS="$TAGS --tag ${DOCKER_IMAGE}:latest" ;;
                            esac
                            CACHE=""
                        else
                            VERSION="dev-$(echo "$GIT_COMMIT" | cut -c1-7)"
                            TAGS="--tag ${DOCKER_IMAGE}:${BRANCH_NAME}"
                            CACHE="--cache-from type=registry,ref=${DOCKER_IMAGE}:buildcache --cache-to type=registry,ref=${DOCKER_IMAGE}:buildcache,mode=max"
                        fi

                        docker buildx build \
                            --file ./Dockerfile \
                            --platform linux/amd64,linux/arm64 \
                            --build-arg VERSION="$VERSION" \
                            --label org.opencontainers.image.version="$VERSION" \
                            --label org.opencontainers.image.revision="$GIT_COMMIT" \
                            --label org.opencontainers.image.created="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
                            $TAGS $CACHE \
                            --push \
                            .
                    '''
                }
            }
            post {
                always {
                    sh 'docker logout "$DOCKER_REGISTRY" || true'
                }
            }
        }

        stage('Release') {
            when {
                tag pattern: 'v*', comparator: 'GLOB'
            }
            agent {
                docker {
                    image 'golang:1.27.2'
                }
            }
            environment {
                HOME = "${WORKSPACE}/.home"
                GOPATH = "${WORKSPACE}/.go"
                GOCACHE = "${WORKSPACE}/.cache/go-build"
            }
            steps {
                sh '''
                    set -eu
                    export CGO_ENABLED=0 GOOS=linux
                    rm -rf dist
                    for arch in amd64 arm64; do
                        GOARCH=$arch go build -o "dist/wings_linux_${arch}" -v -trimpath -ldflags="-s -w -X ${SRC_PATH}/system.Version=${TAG_NAME}" ${SRC_PATH}
                        chmod 755 "dist/wings_linux_${arch}"
                    done
                    (cd dist && sha256sum wings_linux_* > checksums.txt)

                    # The section of the changelog for this version.
                    sed -n "/^## ${TAG_NAME}/,/^## /{/^## /b;p}" CHANGELOG.md > dist/RELEASE_CHANGELOG
                '''
                archiveArtifacts artifacts: 'dist/*', fingerprint: true
            }
        }
    }
}
