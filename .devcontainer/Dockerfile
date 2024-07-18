FROM golang:1.22.5

ARG TERRAFORM_VERSION=1.9.2

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl unzip apt-transport-https lsb-release gnupg make gcc ssh vim

RUN curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - &&\
    apt-get install -y nodejs  && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

RUN curl https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_$(dpkg --print-architecture).zip --output /tmp/terraform.zip && \
    unzip /tmp/terraform.zip -d /bin && \
    rm /tmp/terraform.zip

RUN npm install --global cdktf-cli@latest

WORKDIR /code

ENTRYPOINT [ "bash" ]
