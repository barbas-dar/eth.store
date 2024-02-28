FROM --platform=linux/arm64 golang:1.19-alpine as build

WORKDIR /app

COPY . .

RUN apk add bash build-base libstdc++

RUN go build -o bin/eth.store ./cmd

FROM alpine

RUN apk add libstdc++

COPY --from=build /app/bin/eth.store /bin/eth.store

ENTRYPOINT ["/bin/eth.store"]