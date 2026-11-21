// Inspect audio, or repair only a verified newly downloaded staging copy.
#include <taglib/fileref.h>
#include <taglib/tfile.h>
#include <taglib/tfilestream.h>
#include <taglib/tpropertymap.h>
#include <nlohmann/json.hpp>
#include <iostream>
#include <stdexcept>
#include <string>

using json = nlohmann::json;

static json snapshot(TagLib::FileRef &ref) {
    if(ref.isNull() || !ref.file()->isValid() || !ref.audioProperties())
        throw std::runtime_error("invalid_audio");
    json properties = json::object();
    const auto map = ref.file()->properties();
    for(auto it = map.begin(); it != map.end(); ++it) {
        json values = json::array();
        for(const auto &value : it->second)
            values.push_back(value.to8Bit(true));
        properties[it->first.to8Bit(true)] = values;
    }
    return {{"properties", properties}, {"duration_ms", ref.audioProperties()->lengthInMilliseconds()},
            {"sample_rate", ref.audioProperties()->sampleRate()}, {"channels", ref.audioProperties()->channels()}};
}

int main(int argc, char **argv) {
    try {
        const bool repair = argc == 3 && std::string(argv[1]) == "--repair-download";
        if(argc != 2 && !repair) throw std::runtime_error("invalid_arguments");
        if(!repair && std::cin.peek() != std::char_traits<char>::eof())
            throw std::runtime_error("writes_not_supported");
        TagLib::FileStream stream(argv[repair ? 2 : 1], !repair);
        TagLib::FileRef ref(&stream, true, TagLib::AudioProperties::Accurate);
        if(repair) {
            const auto before = snapshot(ref);
            std::string input;
            char ch;
            while(std::cin.get(ch)) {
                if(input.size() >= 16384) throw std::runtime_error("input_too_large");
                input.push_back(ch);
            }
            const auto data = json::parse(input);
            if(!data.is_object()) throw std::runtime_error("invalid_properties");
            auto properties = ref.file()->properties();
            for(auto it = data.begin(); it != data.end(); ++it) {
                const auto key = it.key();
                if(key != "TITLE" && key != "ARTIST" && key != "ALBUM" && key != "ALBUMARTIST" && key != "TRACKNUMBER" && key != "DISCNUMBER")
                    throw std::runtime_error("invalid_property");
                if(!it.value().is_array() || it.value().size() != 1 || !it.value()[0].is_string())
                    throw std::runtime_error("invalid_value");
                const auto value = it.value()[0].get<std::string>();
                if(value.size() > 1024) throw std::runtime_error("value_too_large");
                properties[TagLib::String(key, TagLib::String::UTF8)] = TagLib::StringList(TagLib::String(value, TagLib::String::UTF8));
            }
            ref.file()->setProperties(properties);
            if(!ref.file()->save()) throw std::runtime_error("save_failed");
            const auto after = snapshot(ref);
            if(before["duration_ms"] != after["duration_ms"] || before["sample_rate"] != after["sample_rate"] || before["channels"] != after["channels"])
                throw std::runtime_error("audio_changed");
        }
        std::cout << snapshot(ref).dump() << '\n';
        return 0;
    } catch(const std::exception &) {
        // Do not echo filenames, tags, process environment, or parser input.
        std::cerr << "taglib_operation_failed\n";
        return 1;
    }
}
