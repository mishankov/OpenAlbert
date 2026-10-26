variable "LOCAL_REPOSITORY" { default = "openalbert-local" }
variable "ARCH" { default = "arm64" }
variable "REVISION" { default = "dev" }
variable "SOURCE" { default = "https://github.com/mishankov/OpenAlbert" }

group "default" {
  targets = ["openalbert", "computer", "stt", "log-gateway", "minio", "minio-init"]
}

target "common" {
  context = "."
  platforms = ["linux/${ARCH}"]
  labels = {
    "org.opencontainers.image.revision" = REVISION
    "org.opencontainers.image.source" = SOURCE
  }
}

target "openalbert" {
  inherits = ["common"]
  dockerfile = "apps/backend/Dockerfile"
  tags = ["${LOCAL_REPOSITORY}:openalbert-${ARCH}"]
}
target "computer" {
  inherits = ["common"]
  dockerfile = "services/computer/Dockerfile"
  tags = ["${LOCAL_REPOSITORY}:computer-${ARCH}"]
}
target "stt" {
  inherits = ["common"]
  dockerfile = "services/stt/Dockerfile"
  tags = ["${LOCAL_REPOSITORY}:stt-${ARCH}"]
}
target "log-gateway" {
  inherits = ["common"]
  dockerfile = "services/log-gateway/Dockerfile"
  tags = ["${LOCAL_REPOSITORY}:log-gateway-${ARCH}"]
}
target "minio" {
  inherits = ["common"]
  dockerfile = "services/minio/Dockerfile"
  target = "minio"
  tags = ["${LOCAL_REPOSITORY}:minio-${ARCH}"]
}
target "minio-init" {
  inherits = ["common"]
  dockerfile = "services/minio/Dockerfile"
  target = "minio-init"
  tags = ["${LOCAL_REPOSITORY}:minio-init-${ARCH}"]
}
