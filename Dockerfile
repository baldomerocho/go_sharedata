FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sharedata .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /sharedata /sharedata
EXPOSE 8844
ENTRYPOINT ["/sharedata"]
