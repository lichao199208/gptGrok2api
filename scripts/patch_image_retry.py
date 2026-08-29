#!/usr/bin/env python3
"""openai_v1_image_generations.handle: retry once on no_image_generated (non-streaming)."""
import py_compile

PATH = "/opt/gptGrok2api/services/protocol/openai_v1_image_generations.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

old_import = "from services.protocol.conversation import (\n    ConversationRequest,\n    collect_image_outputs,\n    count_text_tokens,\n    stream_image_chunks,\n    stream_image_outputs_with_pool,\n)"
new_import = "from services.protocol.conversation import (\n    ConversationRequest,\n    ImageGenerationError,\n    collect_image_outputs,\n    count_text_tokens,\n    stream_image_chunks,\n    stream_image_outputs_with_pool,\n)"
if "ImageGenerationError," not in src:
    if old_import in src:
        src = src.replace(old_import, new_import, 1)
        print("import +")
    else:
        print("import anchor not found")
else:
    print("import exists")

old_body = '''def handle(body: dict[str, Any]) -> dict[str, Any] | Iterator[dict[str, Any]]:
    prompt = str(body.get("prompt") or "")
    model = str(body.get("model") or "gpt-image-2")
    n = int(body.get("n") or 1)
    size = body.get("size")
    quality = str(body.get("quality") or "auto")
    response_format = str(body.get("response_format") or "b64_json")
    base_url = str(body.get("base_url") or "") or None
    progress_callback = body.get("progress_callback")
    outputs = stream_image_outputs_with_pool(ConversationRequest(
        prompt=prompt,
        model=model,
        n=n,
        size=size,
        quality=quality,
        response_format=response_format,
        base_url=base_url,
        message_as_error=True,
        progress_callback=progress_callback,
        call_id=str(body.get("_call_id") or ""),
        trace_image_perf=bool(body.get("_trace_image_perf")),
    ))
    if body.get("stream"):
        input_text_tokens = count_text_tokens(prompt, model)
        return stream_image_chunks(
            outputs,
            event_prefix="image_generation",
            partial_images=body.get("partial_images"),
            usage_builder=lambda data: image_usage(
                input_text_tokens=input_text_tokens,
                output_tokens=count_image_output_items_tokens(data, size, quality),
            ),
        )
    result = collect_image_outputs(outputs)
    result["usage"] = image_usage(
        input_text_tokens=count_text_tokens(prompt, model),
        output_tokens=count_image_output_items_tokens(result.get("data"), size, quality),
    )
    return result'''
new_body = '''def handle(body: dict[str, Any]) -> dict[str, Any] | Iterator[dict[str, Any]]:
    prompt = str(body.get("prompt") or "")
    model = str(body.get("model") or "gpt-image-2")
    n = int(body.get("n") or 1)
    size = body.get("size")
    quality = str(body.get("quality") or "auto")
    response_format = str(body.get("response_format") or "b64_json")
    base_url = str(body.get("base_url") or "") or None
    progress_callback = body.get("progress_callback")

    def _build_request() -> ConversationRequest:
        return ConversationRequest(
            prompt=prompt,
            model=model,
            n=n,
            size=size,
            quality=quality,
            response_format=response_format,
            base_url=base_url,
            message_as_error=True,
            progress_callback=progress_callback,
            call_id=str(body.get("_call_id") or ""),
            trace_image_perf=bool(body.get("_trace_image_perf")),
        )

    if body.get("stream"):
        input_text_tokens = count_text_tokens(prompt, model)
        return stream_image_chunks(
            stream_image_outputs_with_pool(_build_request()),
            event_prefix="image_generation",
            partial_images=body.get("partial_images"),
            usage_builder=lambda data: image_usage(
                input_text_tokens=input_text_tokens,
                output_tokens=count_image_output_items_tokens(data, size, quality),
            ),
        )

    # 非流式：上游完成回合但未产出图片（no_image_generated）时，换账号重试一次。
    last_error: ImageGenerationError | None = None
    for _attempt in range(2):
        try:
            outputs = stream_image_outputs_with_pool(_build_request())
            result = collect_image_outputs(outputs)
            result["usage"] = image_usage(
                input_text_tokens=count_text_tokens(prompt, model),
                output_tokens=count_image_output_items_tokens(result.get("data"), size, quality),
            )
            return result
        except ImageGenerationError as exc:
            last_error = exc
            if exc.code != "no_image_generated":
                raise
    if last_error is not None:
        raise last_error
    raise ImageGenerationError("upstream completed without generating images", code="no_image_generated")'''
if "no_image_generated 时，换账号重试一次" in src:
    print("retry already present")
elif old_body in src:
    src = src.replace(old_body, new_body, 1)
    print("retry added")
else:
    print("body anchor not found")

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)
py_compile.compile(PATH, doraise=True)
print("OK")
