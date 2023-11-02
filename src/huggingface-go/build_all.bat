@echo off
setlocal enabledelayedexpansion

REM 设置你的 Go 项目名称和版本
set project_name=huggingface_go

REM 定义目标平台和架构的数组
set platforms=windows linux darwin
set architectures=amd64 386 arm

REM 编译并为每个目标平台和架构生成可执行文件
for %%p in (%platforms%) do (
    for %%a in (%architectures%) do (
        set platform=%%p
        set architecture=%%a
        set output_name=!project_name!_!platform!_!architecture!

        echo Compiling for !platform! !architecture!
        
        REM 使用 Go 编译
        set GOOS=!platform!
        set GOARCH=!architecture!
        if "!platform!" == "windows" (
            go build -o !output_name!.exe
        ) else (
            go build -o !output_name!
        )

        echo Compilation for !platform! !architecture! completed.
    )
)

echo All compilations completed.
