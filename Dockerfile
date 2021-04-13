FROM ubuntu:18.04

MAINTAINER Quang-Nhat, Hoang-Xuan <hxquangnhat@gmail.com>

COPY kubernetes-auto-ingress /kubernetes-auto-ingress

CMD /kubernetes-auto-ingress
