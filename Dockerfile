# One image, three modes: `gordon receiver`, `gordon worker`, `gordon migrate`.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gordon ./cmd/gordon

# scratch, not distroless: the cluster's rootless BuildKit maps UIDs 0-64535
# only, and distroless ships /home/nonroot owned by 65532, which it cannot
# unpack. A static binary needs no libc, shell or user database.
FROM scratch
# GitHub, Vikunja and the API server are reached over HTTPS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gordon /gordon
# Numeric, so that Kubernetes' runAsNonRoot can check it without a passwd
# file; no file in the image is owned by it.
USER 65532:65532
ENTRYPOINT ["/gordon"]
