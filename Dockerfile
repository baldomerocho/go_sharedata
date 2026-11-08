FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sharedata .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /sharedata /sharedata
USER 65532:65532
EXPOSE 8844
ENTRYPOINT ["/sharedata"]
