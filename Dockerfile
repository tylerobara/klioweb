FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /klioweb .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /klioweb /klioweb
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/klioweb"]
