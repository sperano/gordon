# One image, three modes: `gordon receiver`, `gordon worker`, `gordon migrate`.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gordon ./cmd/gordon

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gordon /gordon
USER nonroot:nonroot
ENTRYPOINT ["/gordon"]
