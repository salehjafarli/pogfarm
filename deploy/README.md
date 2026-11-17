# deploy

Everything needed to build and run the two services as containers:

- Dockerfiles for the pogfarm image, the clipper base image (ffmpeg, TwitchDownloaderCLI, the transcriber and its models) and the clipper image built on top of it
- the compose file that runs both services on one machine for development
- the standalone clipper compose file for running a clipper on any machine

Images are built for both `linux/amd64` and `linux/arm64`.

No secrets or real config belong here. Compose files read them from the environment or from secret files that are not committed.
