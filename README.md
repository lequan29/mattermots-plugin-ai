### This is an automatic translation plugin mattermost
#### Setup:
- Install go: https://go.dev/doc/install
- This template requires node v16 and npm v8
#### Build plugin:
```
make
```
This will produce a single plugin file (with support for multiple architectures) for upload to your Mattermost server:
```
dist/translate-plugin-[version].tar.gz
```